// Package signaling is the rendezvous server: it tracks who is in each room,
// elects the supernode and its backup, watches the supernode's health,
// broadcasts the topology and relays SDP between peers. No media passes
// through it.
package signaling

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"gamecast/internal/proto"
)

const (
	writeTimeout = 5 * time.Second

	// WebSocket ping/pong only cleans up dead connections. Failure detection
	// uses the 250ms status heartbeat below.
	pingInterval = 2 * time.Second
	pongTimeout  = 10 * time.Second

	monitorInterval = 100 * time.Millisecond
	// Host has sent nothing for this long: dead, whatever viewers say.
	hostSilentTimeout = 1500 * time.Millisecond
	// Host quiet for this long AND most viewers' video stopped: dead.
	hostQuietWithStall = 500 * time.Millisecond
	// Host alive and receiving, but most viewers get nothing for this long:
	// it isn't delivering (upload saturated, path broken), so replace it.
	hostNotDelivering = 1500 * time.Millisecond
	// A status older than this is ignored.
	statusFresh = time.Second
	// A host declared dead can't be re-elected for this long.
	suspectFor = 30 * time.Second

	maxSpeedTestBytes = 256 << 20
)

type Server struct {
	Logf func(format string, args ...any)

	mu      sync.Mutex
	rooms   map[string]*room
	nextSeq int

	upgrader websocket.Upgrader
	stop     chan struct{}
	stopOnce sync.Once
}

type peer struct {
	id   string
	seq  int
	join proto.Join
	conn *websocket.Conn
	wmu  sync.Mutex

	// guarded by Server.mu
	lastSeen time.Time
	status   proto.Status
	statusAt time.Time
}

func (p *peer) send(m proto.Message) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return p.conn.WriteJSON(m)
}

type room struct {
	name          string
	peers         map[string]*peer
	streamer      string
	host          string
	backup        string
	epoch         int
	version       int
	epochStreamer string               // streamer the current epoch was built for
	suspects      map[string]time.Time // host ID -> excluded until
	stallSince    time.Time            // when most viewers' video stopped
}

func NewServer() *Server {
	s := &Server{
		Logf:     log.Printf,
		rooms:    map[string]*room{},
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		stop:     make(chan struct{}),
	}
	go s.monitor()
	return s
}

// Close stops the health monitor.
func (s *Server) Close() { s.stopOnce.Do(func() { close(s.stop) }) }

// Handler serves /ws (signaling) and /speedtest (upload measurement).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/speedtest", handleSpeedTest)
	return mux
}

// handleSpeedTest measures how fast the client can push bytes to us.
// It is a rough proxy for the client's upload to the internet.
func handleSpeedTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body := io.LimitReader(r.Body, maxSpeedTestBytes)
	buf := make([]byte, 64<<10)
	var total int64
	var start time.Time
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if start.IsZero() {
				start = time.Now()
			}
			total += int64(n)
		}
		if err != nil {
			break
		}
	}
	elapsed := time.Since(start).Seconds()
	mbps := 0.0
	if elapsed > 0 {
		mbps = float64(total) * 8 / elapsed / 1e6
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"mbps": mbps, "bytes": total})
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	var first proto.Message
	if err := conn.ReadJSON(&first); err != nil || first.Type != proto.TypeJoin || first.Join == nil {
		_ = conn.WriteJSON(proto.Message{Type: proto.TypeError, Error: "first message must be join"})
		return
	}
	j := *first.Join
	if j.Room == "" || (j.Role != proto.RoleStreamer && j.Role != proto.RoleViewer) {
		_ = conn.WriteJSON(proto.Message{Type: proto.TypeError, Error: "join needs room and role streamer|viewer"})
		return
	}

	p, rm, err := s.addPeer(conn, j)
	if err != nil {
		_ = conn.WriteJSON(proto.Message{Type: proto.TypeError, Error: err.Error()})
		return
	}
	s.Logf("[%s] %s joined as %s (%q, up=%.0f Mbps, wired=%v, canHost=%v, voice=%v)",
		j.Room, p.id, j.Role, j.Name, j.UploadMbps, j.Wired, j.CanHost, j.Voice)

	if err := p.send(proto.Message{Type: proto.TypeWelcome, ID: p.id}); err != nil {
		s.removePeer(rm, p)
		return
	}
	s.reconcile(rm)

	_ = conn.SetReadDeadline(time.Now().Add(pongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})
	stopPing := make(chan struct{})
	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-t.C:
				p.wmu.Lock()
				err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout))
				p.wmu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()

	for {
		var m proto.Message
		if err := conn.ReadJSON(&m); err != nil {
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(pongTimeout))
		now := time.Now()
		s.mu.Lock()
		p.lastSeen = now
		if m.Type == proto.TypeStatus && m.Status != nil {
			p.status, p.statusAt = *m.Status, now
		}
		s.mu.Unlock()
		if m.Type == proto.TypeSignal {
			s.relay(rm, p, m)
		}
	}
	close(stopPing)
	s.removePeer(rm, p)
	s.Logf("[%s] %s left", j.Room, p.id)
	s.reconcile(rm)
}

func (s *Server) addPeer(conn *websocket.Conn, j proto.Join) (*peer, *room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rooms[j.Room]
	if r == nil {
		r = &room{name: j.Room, peers: map[string]*peer{}, suspects: map[string]time.Time{}}
		s.rooms[j.Room] = r
	}
	if j.Role == proto.RoleStreamer && r.streamer != "" {
		return nil, nil, fmt.Errorf("room %q already has a streamer", j.Room)
	}
	s.nextSeq++
	p := &peer{id: fmt.Sprintf("p%d", s.nextSeq), seq: s.nextSeq, join: j, conn: conn, lastSeen: time.Now()}
	r.peers[p.id] = p
	if j.Role == proto.RoleStreamer {
		r.streamer = p.id
	}
	return p, r, nil
}

func (s *Server) removePeer(r *room, p *peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(r.peers, p.id)
	delete(r.suspects, p.id)
	if r.streamer == p.id {
		r.streamer = ""
	}
	if len(r.peers) == 0 {
		delete(s.rooms, r.name)
	}
}

func (s *Server) relay(r *room, from *peer, m proto.Message) {
	s.mu.Lock()
	to := r.peers[m.To]
	s.mu.Unlock()
	if to == nil {
		return
	}
	m.From = from.id
	_ = to.send(m)
}

// monitor declares a silent or non-delivering host dead so the warm backup
// takes over, typically within ~0.5-0.8s of a hang.
func (s *Server) monitor() {
	t := time.NewTicker(monitorInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		var failed []*room
		now := time.Now()
		s.mu.Lock()
		for _, r := range s.rooms {
			for id, until := range r.suspects {
				if now.After(until) {
					delete(r.suspects, id)
				}
			}
			if reason := s.checkHost(r, now); reason != "" {
				s.Logf("[%s] host %s declared failed: %s", r.name, r.host, reason)
				r.suspects[r.host] = now.Add(suspectFor)
				r.stallSince = time.Time{}
				failed = append(failed, r)
			}
		}
		s.mu.Unlock()
		for _, r := range failed {
			s.reconcile(r)
		}
	}
}

// checkHost requires s.mu. It returns why the host should be replaced, or "".
func (s *Server) checkHost(r *room, now time.Time) string {
	h := r.peers[r.host]
	if h == nil || r.host == r.streamer || r.suspects[r.host].After(now) {
		r.stallSince = time.Time{}
		return ""
	}
	silent := now.Sub(h.lastSeen)

	// What do the viewers fed by this host see?
	var reporting, stalled int
	for _, p := range r.peers {
		if p == h || p.id == r.streamer || now.Sub(p.statusAt) > statusFresh || p.status.Upstream != h.id {
			continue
		}
		reporting++
		if p.status.State == proto.StateStalled {
			stalled++
		}
	}
	mostStalled := reporting > 0 && stalled*2 >= reporting
	if mostStalled {
		if r.stallSince.IsZero() {
			r.stallSince = now
		}
	} else {
		r.stallSince = time.Time{}
	}
	// If the host itself isn't getting video, the streamer is the problem
	// (game paused, encoder stuck); switching hosts wouldn't help.
	hostReceiving := now.Sub(h.statusAt) <= statusFresh && h.status.State == proto.StateFlowing

	switch {
	case silent > hostSilentTimeout:
		return fmt.Sprintf("no heartbeat for %v", silent.Round(time.Millisecond))
	case mostStalled && silent > hostQuietWithStall:
		return fmt.Sprintf("%d/%d viewers stalled and no heartbeat for %v", stalled, reporting, silent.Round(time.Millisecond))
	case mostStalled && hostReceiving && now.Sub(r.stallSince) > hostNotDelivering:
		return fmt.Sprintf("receiving but not delivering to %d/%d viewers", stalled, reporting)
	}
	return ""
}

// reconcile re-runs the election and pushes the topology to everyone.
func (s *Server) reconcile(r *room) {
	s.mu.Lock()
	var streamer *Candidate
	var viewerCands []*Candidate
	var viewerIDs, voiceIDs []string
	names := map[string]string{}
	bitrate := DefaultBitrateKbps

	all := make([]*peer, 0, len(r.peers))
	for _, p := range r.peers {
		all = append(all, p)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })

	for _, p := range all {
		names[p.id] = p.join.Name
		if p.join.Voice {
			voiceIDs = append(voiceIDs, p.id)
		}
		c := &Candidate{ID: p.id, UploadMbps: p.join.UploadMbps, Wired: p.join.Wired, CanHost: p.join.CanHost, JoinSeq: p.seq}
		if p.id == r.streamer {
			c.IsStreamer, c.CanHost = true, true
			streamer = c
			if p.join.BitrateKbps > 0 {
				bitrate = p.join.BitrateKbps
			}
		} else {
			viewerCands = append(viewerCands, c)
			viewerIDs = append(viewerIDs, p.id)
		}
	}
	suspects := map[string]bool{}
	for id := range r.suspects {
		suspects[id] = true
	}

	d := ElectPair(r.host, r.backup, streamer, viewerCands, bitrate, suspects)
	if d.Host != r.host || r.streamer != r.epochStreamer || r.epoch == 0 {
		r.epoch++
		r.epochStreamer = r.streamer
		s.Logf("[%s] epoch %d: host=%q backup=%q (%s)", r.name, r.epoch, d.Host, d.Backup, d.Reason)
	} else if d.Backup != r.backup {
		s.Logf("[%s] backup=%q", r.name, d.Backup)
	}
	r.host, r.backup = d.Host, d.Backup
	r.version++
	topo := proto.Topology{
		Version:     r.version,
		Epoch:       r.epoch,
		Streamer:    r.streamer,
		Host:        d.Host,
		Backup:      d.Backup,
		Viewers:     viewerIDs,
		Voice:       voiceIDs,
		Names:       names,
		BitrateKbps: bitrate,
		Degraded:    d.Degraded,
		Reason:      d.Reason,
	}
	s.mu.Unlock()

	// Send outside the lock so one slow client can't stall the room.
	msg := proto.Message{Type: proto.TypeTopology, Epoch: topo.Epoch, Topology: &topo}
	for _, p := range all {
		_ = p.send(msg)
	}
}
