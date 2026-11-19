package peer

import (
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
type RTPAudioSource struct {
	conn net.PacketConn
	rtcp net.PacketConn // port+1, where ffmpeg sends RTCP; drained and ignored
}

// ListenRTPAudio listens for Opus RTP on addr (host:port).
func ListenRTPAudio(addr string) (*RTPAudioSource, error) {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("audio input %s: %w", addr, err)
	}
	s := &RTPAudioSource{conn: conn}

	// Encoders send RTCP to port+1. Bind it so they don't get ICMP
	// "port unreachable" errors; ignore failures (the port may be taken).
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
		buf := make([]byte, 1500)
		n, _, err := s.conn.ReadFrom(buf)
		if err != nil {
			return nil, err
		}
		p := &rtp.Packet{}
		if err := p.Unmarshal(buf[:n]); err != nil || p.Version != 2 || len(p.Payload) == 0 {
			continue
		}
		// RTCP multiplexed on the same port shows up as payload types 72-76.
		if p.PayloadType >= 72 && p.PayloadType <= 76 {
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
type audioSink struct {
	mu  sync.Mutex
	w   io.Writer
	ogg *oggwriter.OggWriter
	err error
}

func newAudioSink(w io.Writer) *audioSink { return &audioSink{w: w} }

func (a *audioSink) WriteRTP(p *rtp.Packet) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return
	}
	if a.ogg == nil {
		// Headers are written on creation, so create on the first packet.
		a.ogg, a.err = oggwriter.NewWith(nopCloser{a.w}, OpusCapability.ClockRate, OpusCapability.Channels)
		if a.err != nil {
			return
		}
	}
	_ = a.ogg.WriteRTP(p)
}

// nopCloser keeps the Ogg writer from closing the caller's writer.
type nopCloser struct{ io.Writer }
