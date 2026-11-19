// Package peer is the app-side media core. Every client (streamer or viewer)
// carries the same code, so any of them can be elected supernode or backup
// and start forwarding the stream to the others.
package peer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"gamecast/internal/proto"
)

const (
	keyframeMinInterval = 500 * time.Millisecond // cap: each keyframe inflates bitrate for everyone
	reofferDelay        = time.Second
	statusInterval      = 250 * time.Millisecond
	stallAfter          = 400 * time.Millisecond // no packet for this long = stalled
	neverArrivedAfter   = 5 * time.Second        // active link that never delivered = stalled
)

type Config struct {
	ServerURL string // ws://host:port/ws
	Room      string
	Name      string
	Role      string // proto.RoleStreamer | proto.RoleViewer

	UploadMbps  float64
	Wired       bool
	CanHost     bool // viewer: allow this PC to be the supernode
	BitrateKbps int  // streamer: target bitrate, used for the host election

	ICEServers      []webrtc.ICEServer
	IncludeLoopback bool // tests / single-machine demos

	// Streamer
	Source FrameSource
	FPS    int  // nominal frame rate
	Pace   bool // sleep 1/FPS between frames (for files); live encoders pace themselves

	// Streamer: game audio (optional). Opus RTP packets, e.g. RTPAudioSource.
	Audio AudioSource

	// Viewer: received video as an Annex-B H.264 stream (pipe it to a player).
	Output io.Writer
	// Viewer: received game audio as an Ogg/Opus stream (optional).
	AudioOutput io.Writer

	Logf func(format string, args ...any)
}

type Client struct {
	cfg Config
	api *webrtc.API

	conn *websocket.Conn
	wmu  sync.Mutex

	mu        sync.Mutex
	id        string
	version   int
	topo      *proto.Topology
	links     map[string]*link         // "send:<peer>" / "recv:<peer>"
	wantSend  map[string]bool          // peer -> link carries the live stream
	wantRecv  map[string]bool          // peers I accept a stream from
	pending   map[string]proto.Message // offers that arrived before the topology allowed them
	linkSeq   int
	playSince time.Time
	running   bool
	onChange  []func(proto.Topology)

	// Read on every packet, so atomics instead of mu.
	playFrom atomic.Value // string: peer whose stream I play
	fwdFrom  atomic.Value // string: peer whose stream I forward (the streamer, when I'm host/backup)

	// Streamer: frames are packetized once onto sample and the same packets
	// go to every active link (encode once, send N). Standby links carry
	// idle, which is never written, until a failover swaps the tracks.
	sample *webrtc.TrackLocalStaticSample
	idle   *webrtc.TrackLocalStaticSample
	srcKF  *throttle
	// Game audio, same live/standby scheme. Every link negotiates audio, so
	// these exist even when the streamer has no audio source.
	audio     *webrtc.TrackLocalStaticRTP
	audioIdle *webrtc.TrackLocalStaticRTP

	// Viewer: when host or backup, packets from the streamer are re-sent on
	// fwd / fwdAudio.
	fwd      *webrtc.TrackLocalStaticRTP
	fwdAudio *webrtc.TrackLocalStaticRTP
	out      *outputSink
	audioOut *audioSink

	hung atomic.Bool // test hook: behave like a frozen process
}

func New(cfg Config) (*Client, error) {
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.FPS <= 0 {
		cfg.FPS = 60
	}
	if cfg.Role == proto.RoleStreamer && cfg.Source == nil {
		return nil, errors.New("streamer needs a Source")
	}
	api, err := newAPI(cfg.IncludeLoopback)
	if err != nil {
		return nil, err
	}
	c := &Client{
		cfg: cfg, api: api,
		links:    map[string]*link{},
		wantSend: map[string]bool{},
		wantRecv: map[string]bool{},
		pending:  map[string]proto.Message{},
	}
	c.playFrom.Store("")
	c.fwdFrom.Store("")
	if cfg.Role == proto.RoleStreamer {
		if c.sample, err = webrtc.NewTrackLocalStaticSample(H264Capability, "video", "gamecast"); err != nil {
			return nil, err
		}
		if c.idle, err = webrtc.NewTrackLocalStaticSample(H264Capability, "video", "gamecast"); err != nil {
			return nil, err
		}
		c.srcKF = newThrottle(keyframeMinInterval, cfg.Source.RequestKeyframe)
		if c.audio, err = webrtc.NewTrackLocalStaticRTP(OpusCapability, "audio", "gamecast"); err != nil {
			return nil, err
		}
		if c.audioIdle, err = webrtc.NewTrackLocalStaticRTP(OpusCapability, "audio", "gamecast"); err != nil {
			return nil, err
		}
	} else {
		if c.fwd, err = webrtc.NewTrackLocalStaticRTP(H264Capability, "video", "gamecast"); err != nil {
			return nil, err
		}
		if c.fwdAudio, err = webrtc.NewTrackLocalStaticRTP(OpusCapability, "audio", "gamecast"); err != nil {
			return nil, err
		}
	}
	if cfg.Output != nil {
		c.out = &outputSink{w: cfg.Output}
	}
	if cfg.AudioOutput != nil {
		c.audioOut = newAudioSink(cfg.AudioOutput)
	}
	return c, nil
}

// ID is the peer ID assigned by the server ("" until welcomed).
func (c *Client) ID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.id
}

// Topology returns the latest topology received.
func (c *Client) Topology() (proto.Topology, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.topo == nil {
		return proto.Topology{}, false
	}
	return *c.topo, true
}

// OnTopology registers a callback for topology changes (UI hook).
func (c *Client) OnTopology(f func(proto.Topology)) {
	c.mu.Lock()
	c.onChange = append(c.onChange, f)
	c.mu.Unlock()
}

// Run connects and serves until ctx ends or the connection drops.
func (c *Client) Run(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.cfg.ServerURL, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.cfg.ServerURL, err)
	}
	c.conn = conn
	c.mu.Lock()
	c.running = true
	c.mu.Unlock()
	defer conn.Close()
	defer c.closeAll()

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	join := proto.Join{
		Room: c.cfg.Room, Role: c.cfg.Role, Name: c.cfg.Name,
		UploadMbps: c.cfg.UploadMbps, Wired: c.cfg.Wired, CanHost: c.cfg.CanHost,
		BitrateKbps: c.cfg.BitrateKbps,
	}
	if err := c.send(proto.Message{Type: proto.TypeJoin, Join: &join}); err != nil {
		return err
	}

	if c.cfg.Role == proto.RoleStreamer {
		go c.pumpFrames(ctx)
		if c.cfg.Audio != nil {
			go c.pumpAudio(ctx)
		}
	}
	go c.statusLoop(ctx)

	for {
		var m proto.Message
		if err := conn.ReadJSON(&m); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("signaling: %w", err)
		}
		switch m.Type {
		case proto.TypeWelcome:
			c.mu.Lock()
			c.id = m.ID
			c.mu.Unlock()
			c.cfg.Logf("joined room %q as %s (%s)", c.cfg.Room, m.ID, c.cfg.Role)
		case proto.TypeTopology:
			if m.Topology != nil {
				c.applyTopology(*m.Topology)
			}
		case proto.TypeSignal:
			if m.Signal != nil {
				c.handleSignal(m)
			}
		case proto.TypeError:
			return fmt.Errorf("server: %s", m.Error)
		}
	}
}

func (c *Client) send(m proto.Message) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteJSON(m)
}

// pumpFrames moves encoded frames from the source onto the shared track.
func (c *Client) pumpFrames(ctx context.Context) {
	nominal := time.Second / time.Duration(c.cfg.FPS)
	next := time.Now()
	var last time.Time
	for ctx.Err() == nil {
		f, err := c.cfg.Source.NextFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.cfg.Logf("source: %v", err)
			} else {
				c.cfg.Logf("source ended")
			}
			return
		}
		dur := nominal
		if c.cfg.Pace {
			next = next.Add(nominal)
			if d := time.Until(next); d > 0 {
				time.Sleep(d)
			} else if d < -time.Second {
				next = time.Now() // fell far behind; don't burst to catch up
			}
		} else if !last.IsZero() {
			// Live source: timestamp by arrival so RTP time tracks real time.
			dur = time.Since(last)
			if dur < time.Millisecond || dur > 200*time.Millisecond {
				dur = nominal
			}
		}
		last = time.Now()
		_ = c.sample.WriteSample(media.Sample{Data: f.Data, Duration: dur})
	}
}

// pumpAudio moves Opus packets from the audio source onto the shared audio
// track. Like video, each packet is sent once per live link, never re-encoded.
func (c *Client) pumpAudio(ctx context.Context) {
	for ctx.Err() == nil {
		p, err := c.cfg.Audio.NextPacket()
		if err != nil {
			if ctx.Err() == nil {
				c.cfg.Logf("audio source: %v", err)
			}
			return
		}
		_ = c.audio.WriteRTP(p)
	}
}

// statusLoop is the heartbeat the server uses to detect a failed host.
func (c *Client) statusLoop(ctx context.Context) {
	t := time.NewTicker(statusInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if c.hung.Load() {
			continue
		}
		st := c.status()
		_ = c.send(proto.Message{Type: proto.TypeStatus, Status: &st})
	}
}

func (c *Client) status() proto.Status {
	up := c.playFrom.Load().(string)
	if up == "" {
		return proto.Status{State: proto.StateNone}
	}
	c.mu.Lock()
	l := c.links[recvKey(up)]
	since := c.playSince
	c.mu.Unlock()
	st := proto.Status{Upstream: up, State: proto.StateWaiting}
	if l != nil && l.activeSince.Load() > since.UnixNano() {
		since = time.Unix(0, l.activeSince.Load())
	}
	last := int64(0)
	if l != nil {
		last = l.lastPkt.Load()
	}
	switch {
	case last < since.UnixNano():
		if time.Since(since) > neverArrivedAfter {
			st.State = proto.StateStalled
		}
	case time.Since(time.Unix(0, last)) > stallAfter:
		st.State = proto.StateStalled
	default:
		st.State = proto.StateFlowing
	}
	return st
}

func (c *Client) applyTopology(t proto.Topology) {
	c.mu.Lock()
	if t.Version <= c.version {
		c.mu.Unlock()
		return
	}
	prev := c.topo
	c.version = t.Version
	c.topo = &t
	if prev == nil || prev.Host != t.Host || prev.Backup != t.Backup || prev.Streamer != t.Streamer {
		c.cfg.Logf("topology v%d: I am %s; host=%s backup=%s degraded=%v (%s)",
			t.Version, c.roleLocked(t), t.Host, t.Backup, t.Degraded, t.Reason)
	}
	c.reconcileLocked(t, prev)
	cbs := append([]func(proto.Topology){}, c.onChange...)
	c.mu.Unlock()
	for _, f := range cbs {
		f(t)
	}
}

func (c *Client) roleLocked(t proto.Topology) string {
	me := c.id
	switch {
	case me == t.Streamer && me == t.Host:
		return "STREAMER sending to every viewer"
	case me == t.Streamer && me == t.Backup:
		return "STREAMER (sending to supernode, standby links to every viewer)"
	case me == t.Streamer:
		return "STREAMER (sending to supernode)"
	case me == t.Host:
		return "SUPERNODE"
	case me == t.Backup:
		return "BACKUP supernode (warm standby)"
	}
	return "viewer"
}

// reconcileLocked turns the topology into the set of links this peer needs,
// opening missing ones, closing stale ones and switching which carry media.
//
//	streamer → host            live
//	streamer → backup          standby (idle track; swapped on failover)
//	host     → every viewer    live (forwards what it gets from the streamer)
//	backup   → every viewer    connected, silent until the streamer feeds it
//
// When the streamer itself is host or backup it links to every viewer.
func (c *Client) reconcileLocked(t proto.Topology, prev *proto.Topology) {
	me := c.id
	S, H, B := t.Streamer, t.Host, t.Backup
	send := map[string]bool{}
	recv := map[string]bool{}
	add := func(m map[string]bool, p string, live bool) {
		if p != "" && p != me {
			m[p] = live
		}
	}
	play, fwdFrom := "", ""
	if me == S {
		add(send, H, true)
		add(send, B, H == S)
		if H == S || B == S {
			for _, v := range t.Viewers {
				add(send, v, H == S || v == H)
			}
		}
	} else if me != "" {
		if me == H || me == B {
			add(recv, S, true)
			for _, v := range t.Viewers {
				add(send, v, true)
			}
			fwdFrom = S
		}
		add(recv, H, true)
		add(recv, B, true)
		play = H
		if me == H {
			play = S
		}
	}

	var stale []*link
	for key, l := range c.links {
		if l.send {
			live, ok := send[l.peer]
			if !ok {
				stale = append(stale, l)
				delete(c.links, key)
			} else if me == S && l.live.Load() != live {
				c.setLiveLocked(l, live)
			}
		} else if !recv[l.peer] {
			stale = append(stale, l)
			delete(c.links, key)
		}
	}
	for _, l := range stale {
		l.close()
	}
	for p, live := range send {
		if c.links[sendKey(p)] == nil {
			c.openSendLocked(p, live)
		}
	}
	c.wantSend, c.wantRecv = send, recv
	for p, m := range c.pending {
		if recv[p] {
			delete(c.pending, p)
			c.acceptLocked(m)
		}
	}

	c.fwdFrom.Store(fwdFrom)
	if old := c.playFrom.Load().(string); old != play {
		c.playFrom.Store(play)
		c.playSince = time.Now()
		if l := c.links[recvKey(play)]; l != nil {
			l.startPlayback(c.out)
		}
	}

	// Streamer: the host changed, so viewers are about to decode from a new
	// path. Send a keyframe now rather than waiting for them to ask.
	if me == S && prev != nil && prev.Host != H {
		c.srcKF.Now()
	}
}

// handleSignal routes an offer to accept() or an answer to its send link.
func (c *Client) handleSignal(m proto.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch m.Signal.Kind {
	case "offer":
		if !c.wantRecv[m.From] {
			c.pending[m.From] = m // our topology may lag the sender's
			return
		}
		c.acceptLocked(m)
	case "answer":
		l := c.links[sendKey(m.From)]
		if l == nil || l.id != m.Link {
			return
		}
		go func() {
			if err := l.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.Signal.SDP}); err != nil {
				c.cfg.Logf("answer from %s: %v", m.From, err)
			}
		}()
	}
}

func (c *Client) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running = false
	for key, l := range c.links {
		l.close()
		delete(c.links, key)
	}
}

func (c *Client) newPC() (*webrtc.PeerConnection, error) {
	return c.api.NewPeerConnection(webrtc.Configuration{ICEServers: c.cfg.ICEServers})
}

// simulateHang makes this client behave like a frozen process for tests:
// sockets stay open, but no heartbeats or media leave it.
func (c *Client) simulateHang() { c.hung.Store(true) }

// outputSink serialises writes to the viewer's output across links.
type outputSink struct {
	mu sync.Mutex
	w  io.Writer
}

func (o *outputSink) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.w.Write(p)
}
