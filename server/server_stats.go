package server

import (
	"log"
	"runtime"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// SystemStats is a point-in-time sample of host resource usage.
type SystemStats struct {
	Set         bool    `json:"set"`
	CPU         float64 `json:"cpu"`
	DiskUsed    int64   `json:"diskUsed"`
	DiskTotal   int64   `json:"diskTotal"`
	MemoryUsed  int64   `json:"memoryUsed"`
	MemoryTotal int64   `json:"memoryTotal"`
	GoMemory    int64   `json:"goMemory"`
	GoRoutines  int     `json:"goRoutines"`
}

// sampleSystemStats collects a fresh sample. It is a pure function of the host:
// it neither mutates shared state nor pushes, so the caller decides both.
//
// cpu.Percent(0, false) reports usage since the previous call in this process,
// so the caller's sampling period defines the measurement window.
func sampleSystemStats(diskDir string) SystemStats {
	var s SystemStats
	// Set reports whether *every* source succeeded; a partial sample would
	// otherwise show stale numbers as though they were current.
	ok := true

	if percents, err := cpu.Percent(0, false); err == nil && len(percents) == 1 {
		s.CPU = percents[0]
	} else {
		ok = false
		logStatErr("cpu", err)
	}
	if stat, err := disk.Usage(diskDir); err == nil {
		s.DiskUsed = int64(stat.Used)
		s.DiskTotal = int64(stat.Total)
	} else {
		ok = false
		logStatErr("disk", err)
	}
	if stat, err := mem.VirtualMemory(); err == nil {
		s.MemoryUsed = int64(stat.Used)
		s.MemoryTotal = int64(stat.Total)
	} else {
		ok = false
		logStatErr("memory", err)
	}

	memStats := runtime.MemStats{}
	runtime.ReadMemStats(&memStats)
	s.GoMemory = int64(memStats.Alloc)
	s.GoRoutines = runtime.NumGoroutine()

	s.Set = ok
	return s
}

func logStatErr(what string, err error) {
	if err != nil {
		log.Printf("stats: %s sample failed: %s", what, err)
	}
}
