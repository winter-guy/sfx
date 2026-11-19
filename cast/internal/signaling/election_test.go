package signaling

import "testing"

func viewers(cs ...*Candidate) []*Candidate { return cs }

func TestRequiredUpload(t *testing.T) {
	// 9 viewers at 8 Mbps: streamer sends 9 copies, a viewer-host sends 8.
	if got := RequiredUploadMbps(8000, 9, true); got < 93.5 || got > 93.7 {
		t.Fatalf("streamer need = %v, want 93.6", got)
	}
	if got := RequiredUploadMbps(8000, 9, false); got < 83.1 || got > 83.3 {
		t.Fatalf("viewer-host need = %v, want 83.2", got)
	}
}

func TestElect(t *testing.T) {
	weakStreamer := &Candidate{ID: "s", IsStreamer: true, UploadMbps: 15}
	strongStreamer := &Candidate{ID: "s", IsStreamer: true, UploadMbps: 500}

	fiber := &Candidate{ID: "fiber", UploadMbps: 500, CanHost: true, JoinSeq: 3}
	fiberWifi := &Candidate{ID: "fiberWifi", UploadMbps: 900, CanHost: true, JoinSeq: 1}
	fiberWired := &Candidate{ID: "fiberWired", UploadMbps: 300, Wired: true, CanHost: true, JoinSeq: 2}
	fastNoOptIn := &Candidate{ID: "noOptIn", UploadMbps: 1000, CanHost: false}
	cable := &Candidate{ID: "cable", UploadMbps: 20, CanHost: true}
	plain := &Candidate{ID: "plain"}

	tests := []struct {
		name         string
		current      string
		streamer     *Candidate
		viewers      []*Candidate
		wantHost     string
		wantDegraded bool
	}{
		{"no streamer", "", nil, viewers(fiber), "", false},
		{"no viewers", "", weakStreamer, nil, "s", false},
		{"single viewer goes direct even on weak upload", "", weakStreamer, viewers(fiber), "s", false},
		{"strong streamer serves directly", "", strongStreamer, viewers(fiber, plain, plain), "s", false},
		{"weak streamer uses supernode", "", weakStreamer, viewers(plain, fiber, plain), "fiber", false},
		{"wired beats faster wifi", "", weakStreamer, viewers(fiberWifi, fiberWired, plain), "fiberWired", false},
		{"viewer must opt in", "", weakStreamer, viewers(fastNoOptIn, cable, plain, plain), "cable", true},
		{"keep current host while it qualifies", "fiber", strongStreamer, viewers(fiber, plain, plain), "fiber", false},
		{"drop current host when it no longer qualifies", "cable", weakStreamer, viewers(cable, fiber, plain, plain), "fiber", false},
		{"degraded picks most spare upload", "", weakStreamer, viewers(cable, plain, plain, plain), "cable", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Elect(tc.current, tc.streamer, tc.viewers, 8000, nil)
			if d.Host != tc.wantHost || d.Degraded != tc.wantDegraded {
				t.Fatalf("got host=%q degraded=%v (%s), want host=%q degraded=%v",
					d.Host, d.Degraded, d.Reason, tc.wantHost, tc.wantDegraded)
			}
		})
	}
}

func TestElectPair(t *testing.T) {
	s := &Candidate{ID: "s", IsStreamer: true, UploadMbps: 10}
	a := &Candidate{ID: "a", UploadMbps: 200, Wired: true, CanHost: true, JoinSeq: 1}
	b := &Candidate{ID: "b", UploadMbps: 100, CanHost: true, JoinSeq: 2}
	c := &Candidate{ID: "c", UploadMbps: 100, CanHost: true, JoinSeq: 3}
	x := &Candidate{ID: "x", JoinSeq: 4}

	p := ElectPair("", "", s, viewers(a, b, c, x), 8000, nil)
	if p.Host != "a" || p.Backup != "b" {
		t.Fatalf("initial: got host=%q backup=%q, want a/b", p.Host, p.Backup)
	}

	// a hangs: it is still in the room but suspected. The warm backup b
	// takes over even though c is just as good, and c becomes the backup.
	p = ElectPair("a", "b", s, viewers(a, b, c, x), 8000, map[string]bool{"a": true})
	if p.Host != "b" || p.Backup != "c" {
		t.Fatalf("after hang: got host=%q backup=%q, want b/c", p.Host, p.Backup)
	}

	// a leaves cleanly.
	p = ElectPair("a", "b", s, viewers(b, c, x), 8000, nil)
	if p.Host != "b" || p.Backup != "c" {
		t.Fatalf("after leave: got host=%q backup=%q, want b/c", p.Host, p.Backup)
	}

	// No viewer can take over: the streamer is the backup.
	p = ElectPair("", "", s, viewers(a, x, x), 8000, nil)
	if p.Host != "a" || p.Backup != "s" {
		t.Fatalf("no viewer backup: got host=%q backup=%q, want a/s", p.Host, p.Backup)
	}

	// Streamer hosts directly: no backup.
	strong := &Candidate{ID: "s", IsStreamer: true, UploadMbps: 500}
	p = ElectPair("", "", strong, viewers(a, b, x), 8000, nil)
	if p.Host != "s" || p.Backup != "" {
		t.Fatalf("streamer hosts: got host=%q backup=%q, want s/\"\"", p.Host, p.Backup)
	}
}
