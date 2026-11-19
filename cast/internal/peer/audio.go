package peer

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

// OpusCapability is the game-audio codec: Opus, 48 kHz stereo.
var OpusCapability = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeOpus,
	ClockRate:   48000,
	Channels:    2,
	SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1",
}

const opusPayloadType = 111

// AudioSource yields game audio as Opus RTP packets, one Opus packet each.
// SSRC and payload type are rewritten per link; sequence numbers and
// timestamps are kept as given.
//
// Today that is RTPAudioSource (an encoder such as ffmpeg sending RTP to a
// local UDP port). Native WASAPI loopback / ScreenCaptureKit capture with an
// Opus encoder plugs in here later.
type AudioSource interface {
	NextPacket() (*rtp.Packet, error)
}

// RTPAudioSource receives Opus RTP on a local UDP port, e.g. from
//
//	ffmpeg … -c:a libopus -f rtp -sdp_file audio.sdp rtp://127.0.0.1:5004
//
// NextPacket must be called from one goroutine at a time.
type RTPAudioSource struct {
	conn net.PacketConn
	rtcp net.PacketConn // port+1, where ffmpeg sends RTCP; drained and ignored
	buf  []byte         // reused receive buffer
}

// ListenRTPAudio listens for Opus RTP on addr (host:port), and also absorbs
// the RTCP an encoder like ffmpeg sends to port+1.
func ListenRTPAudio(addr string) (*RTPAudioSource, error) {
	return listenRTP(addr, true)
}

// listenRTP listens for Opus RTP on addr. With rtcp, it also binds port+1 so
// encoders sending RTCP there don't get ICMP "port unreachable" errors; peers
// that never send RTCP (the voice sidecar) don't need the extra socket.
func listenRTP(addr string, rtcp bool) (*RTPAudioSource, error) {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("audio input %s: %w", addr, err)
	}
	s := &RTPAudioSource{conn: conn, buf: make([]byte, 1500)}
	if !rtcp {
		return s, nil
	}

	// Ignore failures: the port may be taken.
	if ua, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		rtcpAddr := net.JoinHostPort(ua.IP.String(), strconv.Itoa(ua.Port+1))
		if rc, err := net.ListenPacket("udp", rtcpAddr); err == nil {
			s.rtcp = rc
			go func() {
				buf := make([]byte, 1500)
				for {
					if _, _, err := rc.ReadFrom(buf); err != nil {
						return
					}
				}
			}()
		}
	}
	return s, nil
}

// Addr is the address being listened on.
func (s *RTPAudioSource) Addr() net.Addr { return s.conn.LocalAddr() }

func (s *RTPAudioSource) NextPacket() (*rtp.Packet, error) {
	for {
		n, _, err := s.conn.ReadFrom(s.buf)
		if err != nil {
			return nil, err
		}
		var h rtp.Header
		hn, err := h.Unmarshal(s.buf[:n])
		// RTCP multiplexed on the same port shows up as payload types 72-76.
		if err != nil || h.Version != 2 || hn >= n || (h.PayloadType >= 72 && h.PayloadType <= 76) {
			continue
		}
		// Accepted: copy out of the reused buffer only now.
		p := &rtp.Packet{}
		if err := p.Unmarshal(bytes.Clone(s.buf[:n])); err != nil || len(p.Payload) == 0 {
			continue
		}
		return p, nil
	}
}

func (s *RTPAudioSource) Close() error {
	if s.rtcp != nil {
		s.rtcp.Close()
	}
	return s.conn.Close()
}

// audioSink writes the played audio as one continuous Ogg/Opus stream.
// Ogg granule positions are derived from the Opus packets, not RTP
// timestamps, so switching supernodes doesn't break the stream.
//
// The first I/O error (reader gone, disk full) stops the sink and is logged
// once. A malformed Opus packet only skips that packet.
type audioSink struct {
	mu   sync.Mutex
	w    *errWriter
	ogg  *oggwriter.OggWriter
	err  error
	logf func(string, ...any)
}

func newAudioSink(w io.Writer, logf func(string, ...any)) *audioSink {
	return &audioSink{w: &errWriter{w: w}, logf: logf}
}

func (a *audioSink) WriteRTP(p *rtp.Packet) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return
	}
	if a.ogg == nil {
		// Headers are written on creation, so create on the first packet.
		if a.ogg, a.err = oggwriter.NewWith(a.w, OpusCapability.ClockRate, OpusCapability.Channels); a.err != nil {
			a.logf("audio output stopped: %v", a.err)
			return
		}
	}
	_ = a.ogg.WriteRTP(p) // bad packets are skipped; I/O errors are caught below
	if a.w.err != nil {
		a.err = a.w.err
		a.logf("audio output stopped: %v", a.err)
	}
}

// errWriter remembers the first write error from the underlying writer. It
// has no Close, which also keeps the Ogg writer from closing the caller's
// writer.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}
