package procstats

import (
	"strings"
	"testing"
	"time"
)

func TestSampleSeesCPUAndMemory(t *testing.T) {
	s := NewSampler()
	// Keep one core busy for ~300ms.
	deadline := time.Now().Add(300 * time.Millisecond)
	x := 0
	for time.Now().Before(deadline) {
		x++
	}
	_ = x
	got := s.Sample()
	t.Log(got)

	if got.CPUPercent < 20 {
		t.Errorf("CPUPercent = %.1f after a busy loop, want > 20", got.CPUPercent)
	}
	if got.GoSysBytes == 0 || got.GoHeapBytes == 0 {
		t.Errorf("Go memory not reported: sys=%d heap=%d", got.GoSysBytes, got.GoHeapBytes)
	}
	if got.RSSBytes == 0 && got.PeakRSSBytes == 0 {
		t.Errorf("no OS memory figure reported")
	}
	if got.Goroutines < 1 || got.NumCPU < 1 {
		t.Errorf("goroutines=%d numCPU=%d", got.Goroutines, got.NumCPU)
	}
	if !strings.Contains(got.String(), "cpu ") {
		t.Errorf("String() = %q", got.String())
	}

	// Idle: should drop well below the busy figure.
	time.Sleep(300 * time.Millisecond)
	if idle := s.Sample(); idle.CPUPercent > got.CPUPercent/2 {
		t.Errorf("idle CPU %.1f%% not below busy %.1f%%", idle.CPUPercent, got.CPUPercent)
	}
}
