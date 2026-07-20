package web

import (
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
	// Started is the formatted process start; Uptime is how long ago that was.
	// The instant itself is deliberately absent: a time.Time rendered raw into
	// markup produces "2026-07-19 10:00:00.123456789 +0000 UTC m=+3.14", and a
	// field no template can reach is one no template can get wrong.
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
		Started:        u.deps.Uptime.Format(time.RFC1123),
		Uptime:         humanSince(u.deps.Uptime),
		MemPercent:     percentOf(d.System.MemoryUsed, d.System.MemoryTotal),
		DiskPercent:    percentOf(d.System.DiskUsed, d.System.DiskTotal),
		DiskFree:       d.System.DiskTotal - d.System.DiskUsed,
	}
	u.emit(statsEvent, "stats", view)
}
