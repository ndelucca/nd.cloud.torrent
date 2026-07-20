// Package sysstat samples host resource usage.
//
// It is a pure reader of the machine: it holds no state, decides nothing about
// when to sample, and is the only place gopsutil is imported. Both the server
// (which publishes it in /api/state) and the renderer (which displays it) use
// this one type, so there is no mapping between a "wire" and a "view" shape.
package sysstat

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// Stats is a point-in-time sample of host resource usage.
//
// The json tags are the /api/state wire contract. They live with the type
// rather than on a server-side copy so that the sampler, the feed and the UI
// cannot drift apart.
type Stats struct {
	Set         bool    `json:"set"`
	CPU         float64 `json:"cpu"`
	DiskUsed    int64   `json:"diskUsed"`
	DiskTotal   int64   `json:"diskTotal"`
	MemoryUsed  int64   `json:"memoryUsed"`
	MemoryTotal int64   `json:"memoryTotal"`
	GoMemory    int64   `json:"goMemory"`
	GoRoutines  int     `json:"goRoutines"`
}

// Sample collects a fresh reading. It is a pure function of the host: it
// neither mutates shared state nor pushes, so the caller decides both.
//
// cpu.Percent(0, false) reports usage since the previous call in this process,
// so the caller's sampling period defines the measurement window.
func Sample(diskDir string) Stats {
	var s Stats
	// Set reports whether *every* source succeeded; a partial sample would
	// otherwise show stale numbers as though they were current.
	ok := true

	if percents, err := cpu.Percent(0, false); err != nil {
		ok = false
		logStatErr("cpu", err)
	} else if len(percents) != 1 {
		// Distinct from the error branch on purpose: logStatErr prints nothing
		// for a nil error, so folding the two together marked the sample
		// untrustworthy with no record of why.
		ok = false
		log.Printf("stats: cpu sample returned %d values, want 1", len(percents))
	} else {
		s.CPU = percents[0]
	}
	if stat, err := disk.Usage(diskTarget(diskDir)); err == nil {
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

// diskTarget resolves dir to the directory whose filesystem to measure.
//
// The download directory is created lazily, by the torrent client's first
// write, so on a fresh install with no torrents it is not there yet and
// disk.Usage reports ENOENT. That one failure cleared Set for the whole sample,
// hiding CPU and memory — neither of which had failed — until the user added
// something.
//
// Substituting the parent is not a way of tolerating the miss but of answering
// the question: the directory will be created there, and a directory lands on
// the filesystem its parent is on, so the parent's free space is exactly what
// "room for downloads" means.
//
// Exactly one level, and only for a missing leaf. Walking further would answer
// confidently about an ancestor with nothing to do with the target: a download
// directory under an unmounted /mnt/bigdisk would report the root filesystem's
// free space as though it were the download disk. A configured path missing
// more than its last component is broken in a way worth reporting, so it is
// left to fail and clear Set.
func diskTarget(dir string) string {
	if dir == "" {
		return "."
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		// The parent has to be there: it is the substitution's whole premise,
		// and its absence is what separates "not created yet" from "configured
		// somewhere that does not exist".
		if parent := filepath.Dir(dir); parent != dir {
			if _, err := os.Stat(parent); err == nil {
				return parent
			}
		}
	}
	return dir
}

func logStatErr(what string, err error) {
	if err != nil {
		log.Printf("stats: %s sample failed: %s", what, err)
	}
}
