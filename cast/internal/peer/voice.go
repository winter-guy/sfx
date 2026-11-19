package peer

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"cast/internal/proto"
)

// VoiceConfig enables voice chat. The audio work (mic capture, echo
// cancellation, Opus, jitter buffering, mixing, playback) runs in the
// voice sidecar (Rust, sonora); cast carries the packets.
type VoiceConfig struct {
	Bin   string   // path to the voice sidecar binary
	Args  []string // extra sidecar arguments, e.g. --input sine:440 --output null
	Muted bool
}

// voiceEngine is a running voice sidecar process.
//
//	sidecar mic ──Opus RTP──▶ mic (UDP) ──▶ voice track ──▶ every voice link
//	voice links ──RTP──▶ voiceAddr (UDP) ──▶ sidecar jitter/mix/speakers
//	game audio  ──RTP──▶ gameAddr  (UDP) ──▶ sidecar (play, or echo reference)
type voiceEngine struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdinMu   sync.Mutex
	mic       *RTPAudioSource
	out       *net.UDPConn
	voiceAddr *net.UDPAddr
	gameAddr  *net.UDPAddr
	done      chan struct{}
}

const voiceStartTimeout = 10 * time.Second

func startVoice(cfg VoiceConfig, gameMode string, logf func(string, ...any)) (*voiceEngine, error) {
	if cfg.Bin == "" {
		return nil, errors.New("voice: no voice sidecar binary configured")
	}
	mic, err := listenRTP("127.0.0.1:0", false) // the sidecar never sends RTCP
	if err != nil {
		return nil, err
	}
	// Our defaults first; caller's Args come later and win (last flag wins).
	args := []string{"--send", mic.Addr().String(), "--game", gameMode}
	if cfg.Muted {
		args = append(args, "--muted")
	}
	args = append(args, cfg.Args...)

	cmd := exec.Command(cfg.Bin, args...)
	stdin, err1 := cmd.StdinPipe()
	stdout, err2 := cmd.StdoutPipe()
	stderr, err3 := cmd.StderrPipe()
	if err := errors.Join(err1, err2, err3); err != nil {
		mic.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		mic.Close()
		return nil, fmt.Errorf("voice: start %s: %w", cfg.Bin, err)
	}
	v := &voiceEngine{cmd: cmd, stdin: stdin, mic: mic, done: make(chan struct{})}

	// Wait closes the pipes, so it must only run after both readers have hit
	// EOF; otherwise the sidecar's last lines (a startup error, its exit
	// stats) can be lost.
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { // sidecar logs → our log
		defer readers.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			logf("%s", sc.Text())
		}
	}()
	ready := make(chan string, 1)
	go func() { // handshake line, then keep draining
		defer readers.Done()
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "READY ") {
				select {
				case ready <- line:
				default:
				}
			}
		}
	}()
	go func() {
		readers.Wait()
		err := cmd.Wait()
		logf("voice engine exited (%v)", err)
		close(v.done)
	}()

	select {
	case line := <-ready:
		if err := v.parseReady(line); err != nil {
			v.close()
			return nil, err
		}
	case <-v.done:
		mic.Close()
		return nil, errors.New("voice: sidecar exited during startup (see log above)")
	case <-time.After(voiceStartTimeout):
		v.close()
		return nil, errors.New("voice: sidecar didn't become ready")
	}
	if v.out, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}); err != nil {
		v.close()
		return nil, err
	}
	return v, nil
}

// parseReady reads "READY voice=127.0.0.1:P game=127.0.0.1:Q".
func (v *voiceEngine) parseReady(line string) error {
	for _, f := range strings.Fields(line)[1:] {
		k, val, _ := strings.Cut(f, "=")
		addr, err := net.ResolveUDPAddr("udp", val)
		if err != nil {
			return fmt.Errorf("voice: bad handshake %q: %w", line, err)
		}
		switch k {
		case "voice":
			v.voiceAddr = addr
		case "game":
			v.gameAddr = addr
		}
	}
	if v.voiceAddr == nil || v.gameAddr == nil {
		return fmt.Errorf("voice: incomplete handshake %q", line)
	}
	return nil
}

func (v *voiceEngine) sendVoice(pkt []byte) { _, _ = v.out.WriteToUDP(pkt, v.voiceAddr) }
func (v *voiceEngine) sendGame(pkt []byte)  { _, _ = v.out.WriteToUDP(pkt, v.gameAddr) }

func (v *voiceEngine) command(c string) {
	v.stdinMu.Lock()
	defer v.stdinMu.Unlock()
	_, _ = io.WriteString(v.stdin, c+"\n")
}

// close asks the sidecar to quit, then kills it if it doesn't.
func (v *voiceEngine) close() {
	v.command("quit")
	v.stdin.Close()
	select {
	case <-v.done:
	case <-time.After(2 * time.Second):
		_ = v.cmd.Process.Kill()
		<-v.done
	}
	v.mic.Close()
	if v.out != nil {
		v.out.Close()
	}
}

// SetVoiceMuted mutes or unmutes this peer's microphone.
func (c *Client) SetVoiceMuted(muted bool) {
	if v := c.voice.Load(); v != nil {
		if muted {
			v.command("mute")
		} else {
			v.command("unmute")
		}
	}
}

// pumpMic sends the sidecar's encoded mic packets on every voice link.
func (c *Client) pumpMic(v *voiceEngine) {
	for {
		p, err := v.mic.NextPacket()
		if err != nil {
			return
		}
		if !c.hung.Load() {
			_ = c.voiceTrack.WriteRTP(p)
		}
	}
}

// Voice links: a full mesh, one sendrecv audio PeerConnection per pair of
// voice peers, independent of the video tree (so a supernode failover never
// interrupts voice). The peer with the smaller ID offers.

func voiceOfferer(me, peer string) bool { return me < peer }

// reconcileVoiceLocked opens/closes voice links for the topology.
func (c *Client) reconcileVoiceLocked(t proto.Topology) {
	me := c.id
	want := map[string]bool{}
	if c.voice.Load() != nil && slices.Contains(t.Voice, me) {
		for _, p := range t.Voice {
			if p != me && p != "" {
				want[p] = true
			}
		}
	}
	for p, l := range c.vlinks {
		if !want[p] {
			l.close()
			delete(c.vlinks, p)
		}
	}
	c.wantVoice = want
	for p := range want {
		if voiceOfferer(me, p) && c.vlinks[p] == nil {
			c.openVoiceLocked(p)
		}
	}
	for p, m := range c.voicePending {
		switch {
		case want[p]:
			delete(c.voicePending, p)
			c.acceptVoiceLocked(m)
		case !slices.Contains(t.Voice, p):
			delete(c.voicePending, p) // sender left the room or voice
		}
	}
}

func (c *Client) openVoiceLocked(peer string) {
	pc, err := c.newPC()
	if err != nil {
		c.cfg.Logf("voice PC for %s: %v", peer, err)
		return
	}
	c.linkSeq++
	l := newLink(fmt.Sprintf("voice/%s-%s#%d", c.id, peer, c.linkSeq), peer, true, pc)
	if l.sender, err = pc.AddTrack(c.voiceTrack); err != nil {
		c.cfg.Logf("voice track for %s: %v", peer, err)
		pc.Close()
		return
	}
	c.vlinks[peer] = l
	c.setupVoicePC(l)
	go c.offer(l)
}

func (c *Client) acceptVoiceLocked(m proto.Message) {
	old := c.vlinks[m.From]
	if old != nil && old.id == m.Link {
		return
	}
	pc, err := c.newPC()
	if err != nil {
		c.cfg.Logf("voice PC: %v", err)
		return
	}
	l := newLink(m.Link, m.From, false, pc)
	if l.sender, err = pc.AddTrack(c.voiceTrack); err != nil {
		c.cfg.Logf("voice track for %s: %v", m.From, err)
		pc.Close()
		return
	}
	c.vlinks[m.From] = l
	c.setupVoicePC(l)
	if old != nil {
		old.close()
	}
	go c.answerVoice(l, m.Signal.SDP)
}

func (c *Client) setupVoicePC(l *link) {
	go drainRTCP(l.sender)
	l.pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		c.receiveVoice(track)
	})
	l.pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		c.cfg.Logf("voice ↔ %s: %s", l.peer, st)
		if st == webrtc.PeerConnectionStateFailed {
			c.retryVoice(l)
		}
	})
}

func (c *Client) answerVoice(l *link, sdp string) {
	pc := l.pc
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		c.cfg.Logf("voice offer from %s: %v", l.peer, err)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		c.cfg.Logf("voice answer: %v", err)
		return
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		c.cfg.Logf("voice set local: %v", err)
		return
	}
	select {
	case <-gathered:
	case <-l.ctx.Done():
		return
	}
	_ = c.send(proto.Message{
		Type: proto.TypeSignal, To: l.peer, Link: l.id,
		Signal: &proto.Signal{Kind: "answer", SDP: pc.LocalDescription().SDP},
	})
}

// retryVoice drops a failed voice link; the offering side builds a new one.
func (c *Client) retryVoice(l *link) {
	c.mu.Lock()
	if c.vlinks[l.peer] == l {
		delete(c.vlinks, l.peer)
	}
	offerer := voiceOfferer(c.id, l.peer)
	c.mu.Unlock()
	l.close()
	if !offerer {
		return
	}
	time.AfterFunc(reofferDelay, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.running && c.wantVoice[l.peer] && c.vlinks[l.peer] == nil {
			c.cfg.Logf("voice: re-offering to %s", l.peer)
			c.openVoiceLocked(l.peer)
		}
	})
}

// handleVoiceSignalLocked routes voice offers/answers (link IDs "voice/…").
func (c *Client) handleVoiceSignalLocked(m proto.Message) {
	switch m.Signal.Kind {
	case "offer":
		if voiceOfferer(c.id, m.From) {
			return // we offer to this peer, not the other way round
		}
		if !c.wantVoice[m.From] {
			c.voicePending[m.From] = m // our topology may lag the sender's
			return
		}
		c.acceptVoiceLocked(m)
	case "answer":
		l := c.vlinks[m.From]
		if l == nil || l.id != m.Link {
			return
		}
		go func() {
			if err := l.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.Signal.SDP}); err != nil {
				c.cfg.Logf("voice answer from %s: %v", m.From, err)
			}
		}()
	}
}

// receiveVoice passes another participant's voice packets to the sidecar
// unchanged; it keys speakers by SSRC.
func (c *Client) receiveVoice(track *webrtc.TrackRemote) {
	buf := make([]byte, 1500)
	for {
		n, _, err := track.Read(buf)
		if err != nil {
			return
		}
		if c.hung.Load() {
			continue
		}
		if v := c.voice.Load(); v != nil {
			v.sendVoice(buf[:n])
		}
	}
}
