package server

import (
	"context"
	"time"

	"github.com/ndelucca/nd.cloud.torrent/files"
	"github.com/ndelucca/nd.cloud.torrent/sysstat"
	"github.com/ndelucca/nd.cloud.torrent/web"
)

// kick asks the render loop to run before its next tick. Without it, pressing
// Start would take up to a full engine.SampleInterval to show any effect.
func (s *Server) kick() {
	select {
	case s.kickCh <- struct{}{}:
	default: // a render is already pending; coalesce
	}
}

// pollLoop renders whatever changed, once per engine sample.
//
// It is driven by engine.Sampled rather than a ticker of its own. Two
// independent 1 Hz timers meant a render could show a sample up to a second
// stale, and the two drifted against each other for no benefit: there is nothing
// new to draw between samples.
//
// A kick can be followed immediately by a pending sample signal, producing two
// renders in quick succession. That is harmless — the renderer is byte-gated, so
// an unchanged region broadcasts nothing.
func (s *Server) pollLoop(ctx context.Context) {
	sampled := s.engine.Sampled()
	for {
		// Gated on watchers because files.List walks the download directory with
		// up to files.Limit stat calls, and rendering for nobody is waste.
		//
		// Torrent *freshness* does not ride on this gate: the engine samples on
		// its own cadence whether or not anyone is connected, so GetTorrents here
		// is a pure read of the latest sample. If this gate ever doubles as the
		// sampling schedule again, the first reading after an idle spell becomes
		// an average over however long nobody was watching.
		if s.watchers() > 0 {
			s.renderStats()
			s.ui.RenderTorrents(s.engine.GetTorrents())
			s.ui.RenderDownloads(files.List(s.downloadDir()))
		}
		select {
		case <-ctx.Done():
			return
		case <-sampled:
		case <-s.kickCh:
			// Floor the rate so a burst of API calls cannot spin this loop.
			select {
			case <-ctx.Done():
				return
			case <-time.After(kickFloor):
			}
		}
	}
}

func (s *Server) statsLoop(ctx context.Context) {
	t := time.NewTicker(statsInterval)
	defer t.Stop()
	for {
		// Sample unconditionally, render only for an audience. cpu.Percent
		// measures since the previous call anywhere in the process, so the
		// interval *is* the measurement window — gating the sample on watchers
		// would make the first reading after an idle spell an average over
		// however long nobody was watching, reported as trustworthy. The sample
		// is one syscall and a ReadMemStats; rendering is what is worth skipping.
		s.stats.set(sysstat.Sample(s.downloadDir()))
		if s.watchers() > 0 {
			s.renderStats()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// watchers counts connected browsers.
func (s *Server) watchers() int { return s.ui.Watchers() }

// renderStats hands the latest sample to the UI.
func (s *Server) renderStats() {
	s.ui.RenderStats(web.StatsData{
		System:         s.stats.get(),
		ConnectedUsers: s.watchers(),
	})
}
