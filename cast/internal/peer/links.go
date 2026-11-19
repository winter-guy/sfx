package peer

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/h264writer"

	"cast/internal/proto"
)

// link is one PeerConnection carrying (or ready to carry) the stream in one
// direction. Links outlive topology changes: a failover only changes which
// links carry media, so no new handshake sits on the critical path.
//
// The side that has (or will have) the stream always sends the offer.
type link struct {
	id   string // chosen by the sender, echoed in the answer
	peer string
	send bool
	pc   *webrtc.PeerConnection
	ctx  context.Context
	stop context.CancelFunc

	// send side
	sender      *webrtc.RTPSender // video
	audioSender *webrtc.RTPSender
	live        atomic.Bool // streamer only: carrying the real tracks, not the idle ones

	// receive side
	ssrc        atomic.Uint32 // video SSRC, for keyframe requests
	lastPkt     atomic.Int64  // unix nanos, video only (drives the stall detector)
	activeSince atomic.Int64  // unix nanos: became the playback source
	writer      atomic.Pointer[h264writer.H264Writer]
	kf          *throttle // PLI to this link's sender
}

func sendKey(peer string) string { return "send:" + peer }
func recvKey(peer string) string { return "recv:" + peer }

func newLink(id, peer string, send bool, pc *webrtc.PeerConnection) *link {
	ctx, stop := context.WithCancel(context.Background())
	return &link{id: id, peer: peer, send: send, pc: pc, ctx: ctx, stop: stop}
}

func (l *link) close() {
	l.stop()
	go l.pc.Close()
}

func (l *link) fresh() bool {
	return time.Since(time.Unix(0, l.lastPkt.Load())) < time.Second
}

func (l *link) sendPLI() {
	ssrc := l.ssrc.Load()
	if ssrc == 0 || l.ctx.Err() != nil {
		return
	}
	_ = l.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
}

// startPlayback makes this link the one we decode from. A fresh H.264 writer
// drops everything until the next keyframe, which we ask for right away.
func (l *link) startPlayback(out *outputSink) {
	if out != nil {
		l.writer.Store(h264writer.NewWith(out))
	}
	l.activeSince.Store(time.Now().UnixNano())
	l.kf.Trigger()
}

// trackFor picks what a send link carries. Streamer: the real track when
// live, an idle (never written) track on standby. Host/backup: the forward
// track, which only has packets while the streamer feeds this peer.
func (c *Client) trackFor(live bool) webrtc.TrackLocal {
	if c.sample != nil {
		if live {
			return c.sample
		}
		return c.idle
	}
	return c.fwd
}

// audioTrackFor is trackFor for game audio.
func (c *Client) audioTrackFor(live bool) webrtc.TrackLocal {
	if c.audio != nil {
		if live {
			return c.audio
		}
		return c.audioIdle
	}
	return c.fwdAudio
}

// setLiveLocked flips a streamer link between standby and live. This is the
// whole failover on the streamer side: no renegotiation, just a track swap.
func (c *Client) setLiveLocked(l *link, live bool) {
	if err := l.sender.ReplaceTrack(c.trackFor(live)); err != nil {
		c.cfg.Logf("send → %s: switch: %v", l.peer, err)
		return
	}
	if err := l.audioSender.ReplaceTrack(c.audioTrackFor(live)); err != nil {
		c.cfg.Logf("send → %s: audio switch: %v", l.peer, err)
	}
	l.live.Store(live)
	state := "standby"
	if live {
		state = "LIVE"
	}
	c.cfg.Logf("send → %s: %s", l.peer, state)
}

func (c *Client) openSendLocked(peer string, live bool) {
	pc, err := c.newPC()
	if err != nil {
		c.cfg.Logf("new PC for %s: %v", peer, err)
		return
	}
	c.linkSeq++
	l := newLink(fmt.Sprintf("%s>%s#%d", c.id, peer, c.linkSeq), peer, true, pc)
	l.live.Store(live)
	if l.sender, err = pc.AddTrack(c.trackFor(live)); err != nil {
		c.cfg.Logf("add track for %s: %v", peer, err)
		pc.Close()
		return
	}
	if l.audioSender, err = pc.AddTrack(c.audioTrackFor(live)); err != nil {
		c.cfg.Logf("add audio track for %s: %v", peer, err)
		pc.Close()
		return
	}
	c.links[sendKey(peer)] = l
	go c.readFeedback(l)
	go drainRTCP(l.audioSender)

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		c.cfg.Logf("send → %s: %s", peer, st)
		switch st {
		case webrtc.PeerConnectionStateConnected:
			c.keyframeFor(l) // a receiver that just connected can't decode until one arrives
		case webrtc.PeerConnectionStateFailed:
			c.retrySend(l)
		}
	})
	go c.offer(l)
}

func (c *Client) offer(l *link) {
	offer, err := l.pc.CreateOffer(nil)
	if err != nil {
		c.cfg.Logf("offer for %s: %v", l.peer, err)
		return
	}
	gathered := webrtc.GatheringCompletePromise(l.pc)
	if err := l.pc.SetLocalDescription(offer); err != nil {
		c.cfg.Logf("set local for %s: %v", l.peer, err)
		return
	}
	select {
	case <-gathered:
	case <-l.ctx.Done():
		return
	}
	_ = c.send(proto.Message{
		Type: proto.TypeSignal, To: l.peer, Link: l.id,
		Signal: &proto.Signal{Kind: "offer", SDP: l.pc.LocalDescription().SDP},
	})
}

// retrySend rebuilds a failed send link if we still want it.
func (c *Client) retrySend(l *link) {
	c.mu.Lock()
	if c.links[sendKey(l.peer)] == l {
		delete(c.links, sendKey(l.peer))
	}
	c.mu.Unlock()
	l.close()
	time.AfterFunc(reofferDelay, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		live, ok := c.wantSend[l.peer]
		if c.running && ok && c.links[sendKey(l.peer)] == nil {
			c.cfg.Logf("re-offering to %s", l.peer)
			c.openSendLocked(l.peer, live)
		}
	})
}

// readFeedback watches RTCP from a receiver for keyframe requests.
func (c *Client) readFeedback(l *link) {
	for {
		pkts, _, err := l.sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				c.keyframeFor(l)
			}
		}
	}
}

// drainRTCP reads (and discards) RTCP so the sender's interceptors keep running.
func drainRTCP(s *webrtc.RTPSender) {
	for {
		if _, _, err := s.ReadRTCP(); err != nil {
			return
		}
	}
}

// keyframeFor handles a keyframe need on a send link: the streamer asks its
// encoder (only for live links), a forwarder asks the streamer, but only
// while it is actually receiving (a silent backup must not cause keyframes).
func (c *Client) keyframeFor(l *link) {
	if c.sample != nil {
		if l.live.Load() {
			c.srcKF.Trigger()
		}
		return
	}
	from := c.fwdFrom.Load().(string)
	if from == "" {
		return
	}
	c.mu.Lock()
	up := c.links[recvKey(from)]
	c.mu.Unlock()
	if up != nil && up.fresh() {
		up.kf.Trigger()
	}
}

// acceptLocked answers an offer and starts a receive link.
func (c *Client) acceptLocked(m proto.Message) {
	key := recvKey(m.From)
	old := c.links[key]
	if old != nil && old.id == m.Link {
		return // duplicate
	}
	pc, err := c.newPC()
	if err != nil {
		c.cfg.Logf("new PC: %v", err)
		return
	}
	l := newLink(m.Link, m.From, false, pc)
	l.kf = newThrottle(keyframeMinInterval, l.sendPLI)
	c.links[key] = l
	if c.playFrom.Load().(string) == m.From {
		l.startPlayback(c.out)
	}
	if old != nil {
		old.close() // sender restarted the link
	}
	go c.answer(l, m.Signal.SDP)
}

func (c *Client) answer(l *link, sdp string) {
	pc := l.pc
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		c.cfg.Logf("recv ← %s: %s", l.peer, st)
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		c.receive(l, track)
	})
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := pc.AddTransceiverFromKind(kind,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			c.cfg.Logf("%s transceiver: %v", kind, err)
			return
		}
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		c.cfg.Logf("offer from %s: %v", l.peer, err)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		c.cfg.Logf("answer: %v", err)
		return
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		c.cfg.Logf("set local: %v", err)
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

// receive reads one incoming stream. Packets from the streamer are forwarded
// untouched (no decode, no re-encode) when this peer is host or backup, and
// written to the player when this link is the playback source.
func (c *Client) receive(l *link, track *webrtc.TrackRemote) {
	if track.Kind() == webrtc.RTPCodecTypeAudio {
		c.receiveAudio(l, track)
		return
	}
	l.ssrc.Store(uint32(track.SSRC()))
	if c.playFrom.Load().(string) == l.peer || c.fwdFrom.Load().(string) == l.peer {
		l.kf.Trigger()
	}
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		if c.hung.Load() {
			continue
		}
		l.lastPkt.Store(time.Now().UnixNano())
		// Header extension IDs were negotiated with the upstream peer and mean
		// nothing downstream; the downstream interceptors add their own.
		pkt.Header.Extension = false
		pkt.Header.Extensions = nil
		if c.fwdFrom.Load().(string) == l.peer {
			_ = c.fwd.WriteRTP(pkt)
		}
		if c.playFrom.Load().(string) == l.peer {
			if w := l.writer.Load(); w != nil {
				_ = w.WriteRTP(pkt)
			}
		}
	}
}

// receiveAudio is receive for game audio: forward when host/backup, play when
// this link is the playback source. Audio doesn't feed the stall detector;
// video is what viewers notice first.
func (c *Client) receiveAudio(l *link, track *webrtc.TrackRemote) {
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		if c.hung.Load() {
			continue
		}
		pkt.Header.Extension = false
		pkt.Header.Extensions = nil
		if c.fwdFrom.Load().(string) == l.peer {
			_ = c.fwdAudio.WriteRTP(pkt)
		}
		if c.playFrom.Load().(string) == l.peer {
			if c.audioOut != nil {
				c.audioOut.WriteRTP(pkt)
			}
			if v := c.voice.Load(); v != nil {
				if b, err := pkt.Marshal(); err == nil {
					v.sendGame(b) // the sidecar plays it and echo-cancels it
				}
			}
		}
	}
}
