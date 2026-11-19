package peer

import (
	"bytes"
	"net"
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
	sink := newAudioSink(&buf)
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
