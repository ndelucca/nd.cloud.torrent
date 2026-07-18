package web

import (
	"log"
	"time"
)

// StatsData is the host sample as the stats region wants it.
//
// It is flat and tag-free, and deliberately not the server's SystemStats: that
// type carries the JSON tags that are the /api/state wire contract, which
// belongs to the server. Mapping between the two costs a handful of field
// copies at the one call site, and buys a boundary where neither side's format
// is hostage to the other's.
type StatsData struct {
	ConnectedUsers int

	// Set reports whether every source of the sample succeeded. A partial
	// sample must not be shown as though it were current.
	Set         bool
	CPU         float64
	DiskUsed    int64
	DiskTotal   int64
	MemoryUsed  int64
	MemoryTotal int64
	GoMemory    int64
	GoRoutines  int
}

// statsView is what the template sees. The percentages are computed here rather
// than in the template: html/template has no arithmetic, and the AngularJS
// version doing `100*used/total` inline was a source of divide-by-zero
// producing +Inf.
type statsView struct {
	StatsData
	Title       string
	Version     string
	Runtime     string
	Uptime      time.Time
	MemPercent  float64
	DiskPercent float64
	DiskFree    int64
}

// RenderStats renders the stats region and broadcasts it if it changed.
func (u *UI) RenderStats(d StatsData) {
	u.mu.Lock()
	defer u.mu.Unlock()

	view := statsView{
		StatsData:   d,
		Title:       u.deps.Title,
		Version:     u.deps.Version,
		Runtime:     u.deps.Runtime,
		Uptime:      u.deps.Uptime,
		MemPercent:  percentOf(d.MemoryUsed, d.MemoryTotal),
		DiskPercent: percentOf(d.DiskUsed, d.DiskTotal),
		DiskFree:    d.DiskTotal - d.DiskUsed,
	}
	frame, err := u.renderer.render("stats", "stats", view)
	if err != nil {
		log.Printf("render stats: %s", err)
		return
	}
	u.hub.broadcast(frame)
}
