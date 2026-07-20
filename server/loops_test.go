package server

import (
	"testing"
	"time"
)

// TestKickDelayFloorsBurstsNotClicks pins the rate limit's shape.
//
// The floor used to be a sleep applied on receipt of every kick, so a single
// click — a Start button with nothing else happening — waited the full
// kickFloor before anything appeared. That is the common case paying to bound
// the rare one. Measuring from the last render instead bounds a burst exactly
// as tightly, which is what the second and third cases below assert together.
func TestKickDelayFloorsBurstsNotClicks(t *testing.T) {
	now := time.Now()

	for _, tc := range []struct {
		name       string
		lastRender time.Time
		want       time.Duration
	}{
		{
			// The isolated click. Nothing has rendered recently, so there is
			// nothing to space this away from.
			name:       "long after the last render",
			lastRender: now.Add(-time.Second),
			want:       0,
		},
		{
			// Mid-burst: a render just happened, so this one waits out the
			// remainder of the window rather than spinning the loop.
			name:       "immediately after a render",
			lastRender: now,
			want:       kickFloor,
		},
		{
			name:       "part way through the window",
			lastRender: now.Add(-kickFloor / 2),
			want:       kickFloor / 2,
		},
		{
			// Exactly at the boundary: the window has elapsed, so no wait.
			name:       "exactly one floor later",
			lastRender: now.Add(-kickFloor),
			want:       0,
		},
		{
			// Nothing has ever rendered, which is the state while no browser is
			// connected. Flooring against the zero instant would compute a wait
			// from the epoch.
			name:       "no render yet",
			lastRender: time.Time{},
			want:       0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := kickDelay(now, tc.lastRender); got != tc.want {
				t.Errorf("kickDelay = %v, want %v", got, tc.want)
			}
		})
	}
}
