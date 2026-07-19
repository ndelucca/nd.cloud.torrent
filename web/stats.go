package web

import (
	"log"
	"time"

	"github.com/ndelucca/nd.cloud.torrent/sysstat"
)

// StatsData is what the stats region is rendered from: the host sample, plus
// the one number that is not a property of the host.
//
// The sample is passed through as sysstat.Stats rather than copied into a view
// shape of its own: a copy means a dozen field assignments kept in lockstep
// with a struct in another package, whose failure mode is a stat that silently
// renders as zero.
type StatsData struct {
	System         sysstat.Stats
	ConnectedUsers int
}

// statsView is what the template sees. The percentages are computed here
// because html/template has no arithmetic — and because doing it inline invites
// `100*used/total`, whose divide-by-zero produces +Inf on a server that has not
// sampled the disk yet.
type statsView struct {
	sysstat.Stats
	ConnectedUsers int
	Version        string
	Runtime        string
	// StartedAt is the process start instant; Started is the formatted form the
	// template shows. A time.Time rendered raw into markup produces
	// "2026-07-19 10:00:00.123456789 +0000 UTC m=+3.14", so the template must
	// never reach for StartedAt directly.
	StartedAt   time.Time
	Started     string
	Uptime      string
	MemPercent  float64
	DiskPercent float64
	DiskFree    int64
}

// RenderStats renders the stats region and broadcasts it if it changed.
func (u *UI) RenderStats(d StatsData) {
	u.mu.Lock()
	defer u.mu.Unlock()

	view := statsView{
		Stats:          d.System,
		ConnectedUsers: d.ConnectedUsers,
		Version:        u.deps.Version,
		Runtime:        u.deps.Runtime,
		StartedAt:      u.deps.Uptime,
		Started:        u.deps.Uptime.Format(time.RFC1123),
		Uptime:         humanSince(u.deps.Uptime),
		MemPercent:     percentOf(d.System.MemoryUsed, d.System.MemoryTotal),
		DiskPercent:    percentOf(d.System.DiskUsed, d.System.DiskTotal),
		DiskFree:       d.System.DiskTotal - d.System.DiskUsed,
	}
	frame, err := u.renderer.render(statsEvent, "stats", view)
	if err != nil {
		log.Printf("render stats: %s", err)
		return
	}
	u.hub.broadcast(frame)
}
