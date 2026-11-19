package peer

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"

	"gamecast/internal/proto"
	"gamecast/internal/signaling"
)

// synthSource emits fake H.264 frames (valid NAL framing, junk payload) at
// ~60 fps, with an SPS/PPS/IDR every gop frames or when asked.
type synthSource struct {
	gop      int
	n        int
	forceKey atomic.Bool
	kfAsked  atomic.Int32
	paused   atomic.Bool
}

func payload(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0x10 + byte((int(seed)+i)%0xE0) // never 0x00/0x01: no fake start codes
	}
	return b
}

func (s *synthSource) NextFrame() (Frame, error) {
	time.Sleep(16 * time.Millisecond)
	for s.paused.Load() {
		time.Sleep(10 * time.Millisecond)
	}
	key := s.n%s.gop == 0 || s.forceKey.Swap(false)
	s.n++
	var b []byte
	if key {
		b = append(b, 0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f, 0x8c, 0x8d)
		b = append(b, 0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80)
		b = append(b, 0, 0, 0, 1, 0x65, 0x88)
		b = append(b, payload(6000, byte(s.n))...) // > MTU: exercises FU-A
	} else {
		b = append(b, 0, 0, 0, 1, 0x41, 0x9a)
		b = append(b, payload(1500, byte(s.n))...)
	}
	return Frame{Data: b, Keyframe: key}, nil
}

func (s *synthSource) RequestKeyframe() {
	s.kfAsked.Add(1)
	s.forceKey.Store(true)
}

// synthAudio emits one 20 ms Opus RTP packet every 20 ms. The payload is a
// valid TOC byte (config 31: 20 ms fullband, stereo, one frame) followed by
// a marker and a counter, so tests can find packets in the Ogg output.
type synthAudio struct {
	seq uint16
	ts  uint32
	n   uint32
}

var audioMarker = []byte("GAMEAUDIO")

func (a *synthAudio) NextPacket() (*rtp.Packet, error) {
	time.Sleep(20 * time.Millisecond)
	a.seq++
	a.ts += 960
	a.n++
	payload := append([]byte{0xFC}, audioMarker...)
	payload = binary.BigEndian.AppendUint32(payload, a.n)
	payload = append(payload, make([]byte, 100)...)
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 97, SequenceNumber: a.seq, Timestamp: a.ts, SSRC: 0xA0D10},
		Payload: payload,
	}, nil
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

func (s *syncBuffer) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Len()
}

var idrMarker = []byte{0, 0, 0, 1, 0x65, 0x88}

type testPeer struct {
	name   string
	c      *Client
	out    *syncBuffer
	audio  *syncBuffer
	cancel context.CancelFunc
}

func eventually(t *testing.T, d time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

type room struct {
	t     *testing.T
	wsURL string
	src   *synthSource
}

func newRoom(t *testing.T) *room {
	srv := signaling.NewServer()
	srv.Logf = logger(t, "server")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { hs.Close(); srv.Close() })
	r := &room{t: t, wsURL: "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws", src: &synthSource{gop: 600}}
	return r
}

// logger returns a Logf for background goroutines. Once the test's cleanup
// starts, logging stops; the lock guarantees no call is half-way through
// t.Logf when the test is marked finished (which the race detector flags).
func logger(t *testing.T, name string) func(string, ...any) {
	var mu sync.Mutex
	done := false
	t.Cleanup(func() {
		mu.Lock()
		done = true
		mu.Unlock()
	})
	return func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			t.Logf(time.Now().Format("05.000")+" ["+name+"] "+f, a...)
		}
	}
}

func (r *room) start(name string, cfg Config) *testPeer {
	return r.startWithLog(name, cfg, nil)
}

func (r *room) startWithLog(name string, cfg Config, capture *logCapture) *testPeer {
	t := r.t
	cfg.ServerURL, cfg.Room, cfg.Name = r.wsURL, "test", name
	cfg.IncludeLoopback = true
	cfg.Logf = logger(t, name)
	if capture != nil {
		cfg.Logf = capture.wrap(cfg.Logf)
	}
	tp := &testPeer{name: name}
	if cfg.Role == proto.RoleViewer {
		tp.out, tp.audio = &syncBuffer{}, &syncBuffer{}
		cfg.Output, cfg.AudioOutput = tp.out, tp.audio
	} else {
		cfg.Source, cfg.Audio = r.src, &synthAudio{}
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tp.c, tp.cancel = c, cancel
	t.Cleanup(cancel)
	go func() { _ = c.Run(ctx) }()
	eventually(t, 5*time.Second, name+" welcomed", func() bool { return c.ID() != "" })
	return tp
}

// standardRoom: a streamer on weak upload (can't serve 3 viewers itself),
// a wired fiber viewer (elected host), a second capable viewer (backup) and
// a plain viewer.
func standardRoom(t *testing.T) (r *room, streamer, fiber, backup, plain *testPeer) {
	r = newRoom(t)
	streamer = r.start("streamer", Config{Role: proto.RoleStreamer, UploadMbps: 10, BitrateKbps: 8000})
	fiber = r.start("fiber", Config{Role: proto.RoleViewer, CanHost: true, UploadMbps: 100, Wired: true})
	backup = r.start("backup", Config{Role: proto.RoleViewer, CanHost: true, UploadMbps: 50})
	plain = r.start("plain", Config{Role: proto.RoleViewer})

	eventually(t, 5*time.Second, "fiber host, backup standby", topoIs(fiber.c.ID(), backup.c.ID(), streamer, fiber, backup, plain))
	for _, v := range []*testPeer{fiber, backup, plain} {
		v := v
		eventually(t, 10*time.Second, v.name+" receives a keyframe via supernode", func() bool {
			return bytes.Contains(v.out.snapshot(), idrMarker)
		})
		eventually(t, 5*time.Second, v.name+" receives game audio via supernode", func() bool {
			return bytes.Count(v.audio.snapshot(), audioMarker) >= 5
		})
	}
	return
}

// audioCount is how many synthetic audio packets reached v's Ogg output.
func audioCount(v *testPeer) int { return bytes.Count(v.audio.snapshot(), audioMarker) }

func topoIs(host, backup string, peers ...*testPeer) func() bool {
	return func() bool {
		for _, p := range peers {
			tp, ok := p.c.Topology()
			if !ok || tp.Host != host || tp.Backup != backup || len(tp.Viewers) < 2 {
				return false
			}
		}
		return true
	}
}

// waitForPicture returns how long (since from) until v's output, past byte
// offset mark, shows a new keyframe followed by at least minBytes of video.
func waitForPicture(t *testing.T, v *testPeer, from time.Time, mark, minBytes int) time.Duration {
	t.Helper()
	eventually(t, 10*time.Second, v.name+" gets a new keyframe", func() bool {
		return bytes.Contains(v.out.snapshot()[mark:], idrMarker)
	})
	took := time.Since(from)
	eventually(t, 5*time.Second, v.name+" keeps receiving", func() bool {
		return v.out.len()-mark > minBytes
	})
	return took
}

func TestFailoverWhenSupernodeHangs(t *testing.T) {
	_, streamer, fiber, backup, plain := standardRoom(t)

	// Frozen process: sockets stay open (no TCP reset), nothing is sent.
	plainMark, backupMark := plain.out.len(), backup.out.len()
	hungAt := time.Now()
	fiber.c.simulateHang()

	took := waitForPicture(t, plain, hungAt, plainMark, 100_000)
	t.Logf("HANG FAILOVER: picture back on plain %v after the supernode froze", took.Round(time.Millisecond))
	if took > 1500*time.Millisecond {
		t.Errorf("failover took %v, want < 1.5s", took)
	}
	// Audio switches with the video: ~25 packets in the next 500 ms.
	before := audioCount(plain)
	eventually(t, 2*time.Second, "game audio continues on plain after failover", func() bool {
		return audioCount(plain)-before >= 20
	})
	// Warm backup took over; with no other capable viewer the streamer is now backup.
	eventually(t, time.Second, "backup promoted", topoIs(backup.c.ID(), streamer.c.ID(), streamer, backup, plain))
	// The new supernode's own player switches too. The keyframe it needs was
	// already requested at failover, so it is in the output by now.
	if !bytes.Contains(backup.out.snapshot()[backupMark:], idrMarker) {
		t.Errorf("new supernode's own player got no keyframe after failover")
	}
}

func TestFailoverWhenSupernodeLeaves(t *testing.T) {
	_, _, fiber, backup, plain := standardRoom(t)

	mark := plain.out.len()
	leftAt := time.Now()
	fiber.cancel()

	took := waitForPicture(t, plain, leftAt, mark, 100_000)
	t.Logf("LEAVE FAILOVER: picture back on plain %v after the supernode left", took.Round(time.Millisecond))
	if took > 500*time.Millisecond {
		t.Errorf("failover took %v, want < 500ms", took)
	}
	if tp, _ := plain.c.Topology(); tp.Host != backup.c.ID() {
		t.Errorf("host = %s, want backup %s", tp.Host, backup.c.ID())
	}
}

func TestStreamerPauseDoesNotTriggerFailover(t *testing.T) {
	r, _, fiber, _, plain := standardRoom(t)

	// Game paused / encoder stuck: everyone stalls, including the host.
	r.src.paused.Store(true)
	time.Sleep(3 * time.Second)
	if tp, _ := plain.c.Topology(); tp.Host != fiber.c.ID() {
		t.Fatalf("host changed to %s during a streamer pause (%s)", tp.Host, tp.Reason)
	}
	mark := plain.out.len()
	r.src.paused.Store(false)
	eventually(t, 3*time.Second, "stream resumes via the same host", func() bool {
		return plain.out.len()-mark > 50_000
	})
}
