package sysstat

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestSampleReadsTheHost pins the happy path: every source succeeded, so Set is
// true and the fields consumers branch on are populated.
//
// The host-dependent half lives in a subtest so that a sandbox which cannot
// provide a source skips *that* and reports it. Skipping from the top level
// instead made the whole test report PASS while having asserted nothing beyond
// the two Go-runtime fields — indistinguishable, in CI output, from a run that
// checked everything.
func TestSampleReadsTheHost(t *testing.T) {
	s := Sample(t.TempDir())

	// The Go-runtime fields cannot fail, so they are asserted unconditionally.
	if s.GoRoutines <= 0 {
		t.Errorf("GoRoutines = %d, want > 0", s.GoRoutines)
	}
	if s.GoMemory <= 0 {
		t.Errorf("GoMemory = %d, want > 0", s.GoMemory)
	}

	t.Run("host sources", func(t *testing.T) {
		if !s.Set {
			// A CI sandbox can legitimately refuse one of the sources. The
			// point of Set is that consumers can tell, and the partial-sample
			// test covers that half.
			t.Skipf("a host source was unavailable; Set=false, sample=%+v", s)
		}
		if s.MemoryTotal <= 0 {
			t.Errorf("MemoryTotal = %d, want > 0 when Set is true", s.MemoryTotal)
		}
		if s.DiskTotal <= 0 {
			t.Errorf("DiskTotal = %d, want > 0 when Set is true", s.DiskTotal)
		}
		if s.MemoryUsed > s.MemoryTotal {
			t.Errorf("MemoryUsed %d exceeds MemoryTotal %d", s.MemoryUsed, s.MemoryTotal)
		}
		if s.DiskUsed > s.DiskTotal {
			t.Errorf("DiskUsed %d exceeds DiskTotal %d", s.DiskUsed, s.DiskTotal)
		}
		if s.CPU < 0 || s.CPU > 100*float64(runtime.NumCPU()) {
			t.Errorf("CPU = %v, outside any plausible range", s.CPU)
		}
	})
}

// TestSampleReportsPartialFailure pins the contract consumers actually branch
// on: one bad source must clear Set rather than pass a partial sample off as
// current. The Go-runtime fields are still filled, which is what makes Set —
// rather than a zero value — the signal.
func TestSampleReportsPartialFailure(t *testing.T) {
	s := Sample("/nonexistent-sysstat-root/nonexistent-leaf")

	if s.Set {
		t.Fatal("a failed disk read must clear Set")
	}
	if s.DiskTotal != 0 {
		t.Errorf("DiskTotal = %d, want 0 for an unreadable path", s.DiskTotal)
	}
	if s.GoRoutines <= 0 {
		t.Error("the Go-runtime fields must still be sampled; Set is the signal, not a zero value")
	}
}

// TestSampleIsPure guards the one property the server depends on when it moved
// sampling off the watchers gate: Sample keeps no state of its own, so two
// consecutive calls both return a populated reading.
func TestSampleIsPure(t *testing.T) {
	dir := t.TempDir()
	first := Sample(dir)
	second := Sample(dir)

	if first.Set != second.Set {
		t.Fatalf("consecutive samples disagree on Set: %v then %v", first.Set, second.Set)
	}
	if second.GoRoutines <= 0 {
		t.Error("the second sample came back empty")
	}
}

// TestSampleSurvivesAMissingDownloadDirectory covers a fresh install: the
// download directory is created lazily by the first write, so before any
// torrent exists disk.Usage reports ENOENT. That single failure used to clear
// Set for the whole sample, so the UI hid CPU and memory too — neither of which
// had failed.
func TestSampleSurvivesAMissingDownloadDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "downloads")

	s := Sample(missing)

	if !s.Set {
		t.Fatal("a not-yet-created download directory is a normal state, not a failed sample")
	}
	if s.DiskTotal == 0 {
		t.Error("disk must be read from the nearest existing ancestor, which is " +
			"the filesystem the directory will land on")
	}
	if s.MemoryTotal == 0 {
		t.Error("memory did not fail and must still be reported")
	}
}

// TestDiskTargetSubstitutesTheParent pins the resolution, and in particular
// where it stops.
//
// One level is the whole design. The fresh-install case is a missing leaf whose
// parent is the filesystem it will be created on, so the parent's reading is
// the right answer. Anything missing above that is a broken configuration —
// a download directory under an unmounted /mnt/bigdisk — and substituting an
// ancestor there would report the root filesystem's free space as though it
// were the download disk. Those must fail instead, so Set clears.
func TestDiskTargetSubstitutesTheParent(t *testing.T) {
	root := t.TempDir()

	if got := diskTarget(filepath.Join(root, "downloads")); got != root {
		t.Errorf("missing leaf must resolve to its parent: got %q, want %q", got, root)
	}
	deep := filepath.Join(root, "a", "b")
	if got := diskTarget(deep); got != deep {
		t.Errorf("a path missing more than its leaf must be left to fail: got %q, want %q", got, deep)
	}
	if got := diskTarget(root); got != root {
		t.Errorf("existing path must be returned as-is: got %q", got)
	}
	if got := diskTarget(""); got != "." {
		t.Errorf("empty path: got %q, want %q", got, ".")
	}
}
