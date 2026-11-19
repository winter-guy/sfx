//go:build windows

package procstats

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func filetimeDuration(ft windows.Filetime) time.Duration {
	// FILETIME durations are in 100ns units.
	return time.Duration((uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)) * 100)
}

func cpuTime() (time.Duration, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err != nil {
		return 0, err
	}
	return filetimeDuration(kernel) + filetimeDuration(user), nil
}

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var procGetProcessMemoryInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

// rss returns the current and peak working set in bytes.
func rss() (cur, peak uint64) {
	var c processMemoryCounters
	c.cb = uint32(unsafe.Sizeof(c))
	r, _, _ := procGetProcessMemoryInfo.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&c)), uintptr(c.cb))
	if r == 0 {
		return 0, 0
	}
	return uint64(c.WorkingSetSize), uint64(c.PeakWorkingSetSize)
}
