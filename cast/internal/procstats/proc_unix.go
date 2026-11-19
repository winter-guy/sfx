//go:build unix

package procstats

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func cpuTime() (time.Duration, error) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, err
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), nil
}

// rss returns current and peak resident memory in bytes.
func rss() (cur, peak uint64) {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		peak = uint64(ru.Maxrss)
		if runtime.GOOS != "darwin" && runtime.GOOS != "ios" {
			peak *= 1024 // Linux and the BSDs report KiB; macOS reports bytes
		}
	}
	if runtime.GOOS == "linux" {
		// statm: size resident shared ... (in pages)
		if b, err := os.ReadFile("/proc/self/statm"); err == nil {
			if f := strings.Fields(string(b)); len(f) > 1 {
				if pages, err := strconv.ParseUint(f[1], 10, 64); err == nil {
					cur = pages * uint64(os.Getpagesize())
				}
			}
		}
	}
	return cur, peak
}
