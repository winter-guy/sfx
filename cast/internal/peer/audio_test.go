package peer

import (
	"bytes"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestRTPAudioSourceReceivesOpusAndSkipsJunk(t *testing.T) {
	src, err := ListenRTPAudio("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	conn, err := net.Dial("udp", src.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	send := func(p *rtp.Packet) {
		b, err := p.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	// Not RTP, RTCP-muxed (PT 72), then a real Opus packet as ffmpeg sends it.
	_, _ = conn.Write([]byte("hello"))
	send(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 72}, Payload: []byte{1, 2, 3}})
	want := []byte{0xFC, 0xAA, 0xBB}
	send(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 97, SequenceNumber: 7, Timestamp: 960}, Payload: want})

	got := make(chan *rtp.Packet, 1)
	go func() {
		p, err := src.NextPacket()
		if err == nil {
			got <- p
		}
	}()
	select {
	case p := <-got:
		if !bytes.Equal(p.Payload, want) || p.SequenceNumber != 7 || p.Timestamp != 960 {
			t.Fatalf("got seq=%d ts=%d payload=%x", p.SequenceNumber, p.Timestamp, p.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no packet received")
	}
}

func TestAudioSinkWritesOggOpus(t *testing.T) {
	var buf bytes.Buffer
	sink := newAudioSink(&buf, func(string, ...any) {})
	a := &synthAudio{}
	for i := 0; i < 3; i++ {
		p, _ := a.NextPacket()
		sink.WriteRTP(p)
	}
	out := buf.Bytes()
	if !bytes.HasPrefix(out, []byte("OggS")) || !bytes.Contains(out, []byte("OpusHead")) {
		t.Fatalf("not an Ogg/Opus stream: % x", out[:min(len(out), 40)])
	}
	if n := bytes.Count(out, audioMarker); n != 3 {
		t.Fatalf("found %d audio packets in output, want 3", n)
	}
}

// failAfter accepts n writes, then fails every write.
type failAfter struct {
	n, calls int
}

func (f *failAfter) Write(p []byte) (int, error) {
	f.calls++
	if f.calls > f.n {
		return 0, errors.New("broken pipe")
	}
	return len(p), nil
}

func TestAudioSinkStopsOnWriteErrorAndLogsOnce(t *testing.T) {
	w := &failAfter{n: 3} // Ogg headers + one packet, then the reader goes away
	var logs []string
	sink := newAudioSink(w, func(f string, a ...any) { logs = append(logs, f) })
	a := &synthAudio{}
	for i := 0; i < 10; i++ {
		p, _ := a.NextPacket()
		sink.WriteRTP(p)
	}
	if len(logs) != 1 {
		t.Fatalf("logged %d times, want once: %q", len(logs), logs)
	}
	if w.calls != 4 {
		t.Fatalf("writer called %d times; after the first failure it should stop", w.calls)
	}
}

func TestAudioSinkSkipsMalformedPacket(t *testing.T) {
	var buf bytes.Buffer
	var logs []string
	sink := newAudioSink(&buf, func(f string, a ...any) { logs = append(logs, f) })
	a := &synthAudio{}
	p, _ := a.NextPacket()
	sink.WriteRTP(p)
	bad := *p
	bad.Payload = []byte{0x03} // code 3 with no frame-count byte: invalid Opus
	sink.WriteRTP(&bad)
	p2, _ := a.NextPacket()
	sink.WriteRTP(p2)
	if n := bytes.Count(buf.Bytes(), audioMarker); n != 2 || len(logs) != 0 {
		t.Fatalf("got %d packets and logs %q; a bad packet must not stop the sink", n, logs)
	}
}

func TestRTCPPortOnlyBoundWhenAsked(t *testing.T) {
	rtcpFree := func(s *RTPAudioSource) bool {
		port := s.Addr().(*net.UDPAddr).Port + 1
		c, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	withRTCP, err := ListenRTPAudio("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer withRTCP.Close()
	if rtcpFree(withRTCP) {
		t.Error("ListenRTPAudio should bind port+1 for the encoder's RTCP")
	}
	noRTCP, err := listenRTP("127.0.0.1:0", false)
	if err != nil {
		t.Fatal(err)
	}
	defer noRTCP.Close()
	if !rtcpFree(noRTCP) {
		t.Error("listenRTP(rtcp=false) must not bind port+1")
	}
}
