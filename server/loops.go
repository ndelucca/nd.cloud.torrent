package server

import (
	"context"
	"time"

	"github.com/ndelucca/nd.cloud.torrent/engine"
	"github.com/ndelucca/nd.cloud.torrent/files"
	"github.com/ndelucca/nd.cloud.torrent/sysstat"
	"github.com/ndelucca/nd.cloud.torrent/web"
)

// renderLoop decides *when* the UI is redrawn. What a region contains belongs to
// web; where the data comes from belongs to the engine and the filesystem. This
// holds only the schedule.
//
// It is its own type rather than a cluster of Server fields because the policy
// here — follow the engine's sample, floor the event-driven renders, skip the
// work entirely when nobody is watching — is the part most likely to change,
// and as methods on Server none of it could be exercised without a config file,
// a listening socket and a torrent client.
type renderLoop struct {
	ui          *web.UI
	engine      *engine.Engine
	downloadDir func() string
	stats       *sampledStats

	// kickCh is buffered and coalesced: a burst of API calls between two ticks
	// costs one extra render, not one per call.
	kickCh chan struct{}

	// now is time.Now, replaced in tests. A seam rather than a parameter: the
	// floor it drives is this type's own policy, and threading a clock through
	// Run would put it in the server's contract for no other reason.
	now func() time.Time
}

func newRenderLoop(ui *web.UI, e *engine.Engine, downloadDir func() string, stats *sampledStats) *renderLoop {
	return &renderLoop{
		ui:          ui,
		engine:      e,
		downloadDir: downloadDir,
		stats:       stats,
		kickCh:      make(chan struct{}, 1),
		now:         time.Now,
	}
}

// kick asks for a render before the next tick. Without it, pressing Start would
// take up to a full engine.SampleInterval to show any effect.
func (l *renderLoop) kick() {
	select {
	case l.kickCh <- struct{}{}:
	default: // a render is already pending; coalesce
	}
}

// watchers counts connected browsers.
func (l *renderLoop) watchers() int { return l.ui.Watchers() }

// renderStats hands the latest sample to the UI.
func (l *renderLoop) renderStats() {
	l.ui.RenderStats(web.StatsData{
		System:         l.stats.get(),
		ConnectedUsers: l.watchers(),
	})
}

// render draws every region and records when it happened.
//
// Gated on watchers because files.List walks the download directory with up to
// files.Limit stat calls, and rendering for nobody is waste.
//
// Torrent *freshness* does not ride on that gate: the engine samples on its own
// cadence whether or not anyone is connected, so GetTorrents here is a pure read
// of the latest sample. If this gate ever doubles as the sampling schedule
// again, the first reading after an idle spell becomes an average over however
// long nobody was watching.
func (l *renderLoop) render() bool {
	if l.watchers() == 0 {
		return false
	}
	l.renderStats()
	l.ui.RenderTorrents(l.engine.GetTorrents())
	l.ui.RenderDownloads(files.List(l.downloadDir()))
	return true
}

// kickDelay reports how long a kick received at now must wait, given when the
// last render actually happened.
//
// The floor is measured from the last render rather than slept on receipt.
// Sleeping unconditionally charged every isolated click the full kickFloor
// before anything appeared on screen — the common case paying to bound the rare
// one. Measuring from the last render bounds a burst exactly as tightly and
// leaves a lone click immediate.
//
// A zero lastRender means nothing has been drawn yet, which is the state while
// no browser is connected: there is no render to space this one away from.
func kickDelay(now, lastRender time.Time) time.Duration {
	if lastRender.IsZero() {
		return 0
	}
	if d := kickFloor - now.Sub(lastRender); d > 0 {
		return d
	}
	return 0
}

// poll renders whatever changed, once per engine sample.
//
// It is driven by engine.Sampled rather than a ticker of its own. Two
// independent 1 Hz timers meant a render could show a sample up to a second
// stale, and the two drifted against each other for no benefit: there is
// nothing new to draw between samples.
//
// A kick can be followed immediately by a pending sample signal, producing two
// renders in quick succession. That is harmless — the renderer is byte-gated, so
// an unchanged region broadcasts nothing.
func (l *renderLoop) poll(ctx context.Context) {
	sampled := l.engine.Sampled()
	var lastRender time.Time
	for {
		if l.render() {
			lastRender = l.now()
		}

		select {
		case <-ctx.Done():
			return
		case <-sampled:
		case <-l.kickCh:
			if wait := kickDelay(l.now(), lastRender); wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
		}
	}
}

// sampleHost takes a host reading on a fixed cadence and redraws the stats
// region for whoever is watching.
func (l *renderLoop) sampleHost(ctx context.Context) {
	t := time.NewTicker(statsInterval)
	defer t.Stop()
	for {
		// Sample unconditionally, render only for an audience. cpu.Percent
		// measures since the previous call anywhere in the process, so the
		// interval *is* the measurement window — gating the sample on watchers
		// would make the first reading after an idle spell an average over
		// however long nobody was watching, reported as trustworthy. The sample
		// is one syscall and a ReadMemStats; rendering is what is worth skipping.
		l.stats.set(sysstat.Sample(l.downloadDir()))
		if l.watchers() > 0 {
			l.renderStats()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
