package peer

import (
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// H264Capability is the one codec every peer negotiates. Both ends are this
// app, so we pin a single profile string; the bitstream itself may be any
// profile the hardware encoder produces (Pion forwards it untouched).
var H264Capability = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeH264,
	ClockRate:   90000,
	SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
}

const h264PayloadType = 102

func newAPI(includeLoopback bool) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: H264Capability,
		PayloadType:        h264PayloadType,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: OpusCapability,
		PayloadType:        opusPayloadType,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}

	// NACK (retransmissions), RTCP reports and TWCC. On a supernode this means
	// a viewer's lost packet is re-sent by the host, not the streamer.
	reg := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		return nil, err
	}

	se := webrtc.SettingEngine{}
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	// Detect a dead link in ~3s instead of Pion's default 5s/25s.
	se.SetICETimeouts(3*time.Second, 6*time.Second, time.Second)
	se.SetIncludeLoopbackCandidate(includeLoopback)

	return webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(reg),
		webrtc.WithSettingEngine(se),
	), nil
}

// throttle runs fn at most once per interval. A call inside the window is
// not lost: it is deferred to the end of the window (trailing edge), so a
// viewer that joins right after a keyframe still gets one shortly.
type throttle struct {
	every time.Duration
	fn    func()

	mu      sync.Mutex
	last    time.Time
	pending bool
}

func newThrottle(every time.Duration, fn func()) *throttle {
	return &throttle{every: every, fn: fn}
}

func (t *throttle) Trigger() {
	t.mu.Lock()
	wait := t.every - time.Since(t.last)
	if wait <= 0 {
		t.last = time.Now()
		t.mu.Unlock()
		t.fn()
		return
	}
	if t.pending {
		t.mu.Unlock()
		return
	}
	t.pending = true
	t.mu.Unlock()
	time.AfterFunc(wait, func() {
		t.mu.Lock()
		t.pending = false
		t.last = time.Now()
		t.mu.Unlock()
		t.fn()
	})
}

// Now runs fn immediately regardless of the window (failover must not wait).
func (t *throttle) Now() {
	t.mu.Lock()
	t.last = time.Now()
	t.mu.Unlock()
	t.fn()
}
