// Package procstats reports what the cast process itself consumes:
// CPU, memory and goroutines. It has no cgo and no extra dependencies.
package procstats

import (
	"context"
	"fmt"
	"runtime"
	"time"
)

// Sample is one measurement of this process.
type Sample struct {
	// CPUPercent is CPU time used since the previous sample as a share of
	// one core: 100 means one core fully busy, 250 means 2.5 cores.
	CPUPercent float64
	NumCPU     int

	// RSSBytes is current resident memory as reported by the OS. 0 where it
	// isn't cheaply available (macOS without cgo; use PeakRSSBytes there).
	RSSBytes     uint64
	PeakRSSBytes uint64 // highest RSS so far, 0 if unknown

	GoSysBytes  uint64 // memory the Go runtime obtained from the OS
	GoHeapBytes uint64 // live heap objects
	Goroutines  int
}

// Sampler computes CPU usage between consecutive calls.
type Sampler struct {
	lastCPU  time.Duration
	lastWall time.Time
}

// NewSampler starts measuring from now.
func NewSampler() *Sampler {
	cpu, _ := cpuTime()
	return &Sampler{lastCPU: cpu, lastWall: time.Now()}
}

// Sample measures the process now.
func (s *Sampler) Sample() Sample {
	now := time.Now()
	cpu, err := cpuTime()
	out := Sample{NumCPU: runtime.NumCPU(), Goroutines: runtime.NumGoroutine()}
	if wall := now.Sub(s.lastWall); err == nil && wall > 0 {
		out.CPUPercent = 100 * float64(cpu-s.lastCPU) / float64(wall)
	}
	s.lastCPU, s.lastWall = cpu, now

	out.RSSBytes, out.PeakRSSBytes = rss()

	var m runtime.MemStats
	runtime.ReadMemStats(&m) // brief stop-the-world; fine every few seconds
	out.GoSysBytes, out.GoHeapBytes = m.Sys, m.HeapAlloc
	return out
}

func mb(b uint64) string { return fmt.Sprintf("%.1f MB", float64(b)/(1<<20)) }

func (s Sample) String() string {
	mem := ""
	switch {
	case s.RSSBytes > 0:
		mem = "rss " + mb(s.RSSBytes)
		if s.PeakRSSBytes > 0 {
			mem += " (peak " + mb(s.PeakRSSBytes) + ")"
		}
	case s.PeakRSSBytes > 0:
		mem = "peak rss " + mb(s.PeakRSSBytes)
	}
	if mem != "" {
		mem += ", "
	}
	return fmt.Sprintf("cpu %.1f%% of 1 core (%.1f%% of %d) | %sgo sys %s, heap %s | goroutines %d",
		s.CPUPercent, s.CPUPercent/float64(s.NumCPU), s.NumCPU,
		mem, mb(s.GoSysBytes), mb(s.GoHeapBytes), s.Goroutines)
}

// Log writes a Sample every interval until ctx ends.
func Log(ctx context.Context, every time.Duration, logf func(string, ...any)) {
	if every <= 0 {
		return
	}
	s := NewSampler()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			logf("stats: %s", s.Sample())
		}
	}
}
