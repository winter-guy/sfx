package peer

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gamecast/internal/proto"
)

// voiceBin finds the gamecast-voice sidecar, or skips the test.
func voiceBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("GAMECAST_VOICE_BIN"); p != "" {
		return p
	}
	p, _ := filepath.Abs("../../voice/target/debug/gamecast-voice")
	if _, err := os.Stat(p); err != nil {
		t.Skip("voice sidecar not built: run `cargo build` in voice/ or set GAMECAST_VOICE_BIN")
	}
	return p
}

// tonePower is the normalised power of freq in the left channel of a raw
// 48 kHz stereo float32 file, over its last `seconds`.
func tonePower(t *testing.T, path string, freq float64, seconds float64) float64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frames := len(b) / 8
	n := int(seconds * 48000)
	if frames < n {
		return 0
	}
	coeff := 2 * math.Cos(2*math.Pi*freq/48000)
	var s1, s2 float64
	for i := frames - n; i < frames; i++ {
		x := float64(math.Float32frombits(binary.LittleEndian.Uint32(b[i*8:])))
		s := x + coeff*s1 - s2
		s2, s1 = s1, s
	}
	return (s1*s1 + s2*s2 - coeff*s1*s2) / float64(n*n)
}

func db(a, b float64) float64 { return 10 * math.Log10(a/math.Max(b, 1e-15)) }

type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) wrap(next func(string, ...any)) func(string, ...any) {
	return func(f string, a ...any) {
		l.mu.Lock()
		l.lines = append(l.lines, strings.TrimSpace(fmt.Sprintf(f, a...)))
		l.mu.Unlock()
		next(f, a...)
	}
}

func (l *logCapture) find(re *regexp.Regexp) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.lines) - 1; i >= 0; i-- {
		if m := re.FindStringSubmatch(l.lines[i]); m != nil {
			return m
		}
	}
	return nil
}

// voiceRoom runs a streamer and two voice viewers: A says 440 Hz; B is
// silent but its speakers leak into its mic (echo). Returns how loud A is at
// B, and how loud A's own echo comes back to A.
func voiceRoom(t *testing.T, aecOnB bool) (aAtB, echoAtA float64, a, b *testPeer, aRaw, bRaw string) {
	bin := voiceBin(t)
	dir := t.TempDir()
	aRaw, bRaw = filepath.Join(dir, "a.raw"), filepath.Join(dir, "b.raw")
	r := newRoom(t)
	r.start("streamer", Config{Role: proto.RoleStreamer, UploadMbps: 10, BitrateKbps: 8000})

	common := []string{"--no-ns", "--no-agc", "--game", "off"} // NS would treat a steady tone as noise
	a = r.start("A", Config{Role: proto.RoleViewer, Voice: &VoiceConfig{Bin: bin,
		Args: append(append([]string{}, common...), "--input", "sine:440", "--output", "raw:"+aRaw)}})
	bArgs := append(append([]string{}, common...), "--input", "silence", "--output", "raw:"+bRaw, "--echo-sim", "0.5:30")
	if !aecOnB {
		bArgs = append(bArgs, "--no-aec")
	}
	b = r.start("B", Config{Role: proto.RoleViewer, Voice: &VoiceConfig{Bin: bin, Args: bArgs}})

	eventually(t, 10*time.Second, "B hears A", func() bool { return tonePower(t, bRaw, 440, 0.5) > 1e-3 })
	time.Sleep(5 * time.Second) // let the echo canceller converge
	return tonePower(t, bRaw, 440, 2), tonePower(t, aRaw, 440, 2), a, b, aRaw, bRaw
}

func TestVoiceChatEchoCancellationEndToEnd(t *testing.T) {
	_, echoNoAEC, _, _, _, _ := voiceRoom(t, false)
	aAtB, echoAEC, a, _, _, bRaw := voiceRoom(t, true)

	t.Logf("A at B: %.2e | A's echo back at A: without AEC %.2e, with AEC %.2e (%.1f dB less)",
		aAtB, echoNoAEC, echoAEC, db(echoNoAEC, echoAEC))
	if aAtB < 1e-3 {
		t.Errorf("B should clearly hear A (power %.2e)", aAtB)
	}
	if echoNoAEC < 1e-4 {
		t.Errorf("control: without AEC, A should hear its own echo (power %.2e)", echoNoAEC)
	}
	if d := db(echoNoAEC, echoAEC); d < 20 {
		t.Errorf("echo reduced by only %.1f dB end to end, want ≥ 20 dB", d)
	}

	// Mute: A's voice disappears at B.
	a.c.SetVoiceMuted(true)
	time.Sleep(time.Second)
	if muted := tonePower(t, bRaw, 440, 0.5); db(aAtB, muted) < 20 {
		t.Errorf("after mute B still hears A: %.2e vs %.2e", muted, aAtB)
	}
}

func TestVoiceSidecarGetsGameAudio(t *testing.T) {
	bin := voiceBin(t)
	r := newRoom(t)
	r.start("streamer", Config{Role: proto.RoleStreamer, UploadMbps: 100, BitrateKbps: 8000})
	logs := &logCapture{}
	v := r.startWithLog("viewer", Config{Role: proto.RoleViewer, Voice: &VoiceConfig{Bin: bin,
		Args: []string{"--input", "silence", "--output", "null"}}}, logs)
	time.Sleep(2 * time.Second) // ~100 game-audio packets at 50/s
	v.cancel()                  // sidecar prints its stats on exit
	stats := regexp.MustCompile(`voice: stats voice_rx=\d+ game_rx=(\d+)`)
	eventually(t, 5*time.Second, "sidecar exit stats", func() bool { return logs.find(stats) != nil })
	n, _ := strconv.Atoi(logs.find(stats)[1])
	if n < 50 {
		t.Errorf("sidecar received %d game-audio packets in 2s, want ≥ 50", n)
	}
}
