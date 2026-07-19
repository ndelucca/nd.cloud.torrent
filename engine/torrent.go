package engine

import (
	"slices"
	"time"

	"github.com/anacrolix/torrent"
)

// Torrent is the view model handed to the server and marshalled straight to the
// browser. Exported field names are part of the UI contract.
type Torrent struct {
	//anacrolix/torrent
	InfoHash   string
	Name       string
	Loaded     bool
	Downloaded int64
	Size       int64
	Files      []File
	//cloud torrent
	Started      bool
	Percent      float32
	DownloadRate float32
	//internal — never leaves the engine
	t         *torrent.Torrent
	spec      *torrent.TorrentSpec
	updatedAt time.Time
}

// File is the per-file progress view. It is a value, not a pointer, so no
// consumer has to nil-check an entry.
type File struct {
	//anacrolix/torrent
	Path string
	Size int64
	//cloud torrent
	Percent float32
}

// clone returns a deep copy safe to hand to another goroutine. The internal
// handles are deliberately dropped: callers outside the engine must not be able
// to reach the underlying client.
func (t *Torrent) clone() *Torrent {
	c := *t
	c.t = nil
	c.spec = nil
	c.Files = slices.Clone(t.Files)
	return &c
}

// update copies live state out of the underlying torrent. Callers must hold the
// engine mutex.
//
// now is passed in rather than read here so one pass shares a single timestamp,
// and so a test can drive a deterministic two-sample sequence without a real
// clock or a real download.
func (t *Torrent) update(tt *torrent.Torrent, now time.Time) {
	t.Name = tt.Name()
	t.Loaded = tt.Info() != nil
	if t.Loaded {
		t.updateLoaded(tt, now)
	}
	t.t = tt
}

func (t *Torrent) updateLoaded(tt *torrent.Torrent, now time.Time) {
	// Rebuilt each pass rather than patched in place: keeping entries by index
	// assumes index i is the same file across ticks, which stops being true once
	// a torrent is re-added. Every field is read from the live torrent anyway, so
	// preserving nothing costs one small allocation per torrent per tick.
	tfiles := tt.Files()
	t.Files = make([]File, len(tfiles))
	for i, f := range tfiles {
		t.Files[i] = File{
			Path:    f.Path(),
			Size:    f.Length(),
			Percent: percent(f.BytesCompleted(), f.Length()),
		}
	}

	t.Size = tt.Length()
	t.sample(tt.BytesCompleted(), now)
}

// sample records a progress reading and derives the download rate from it.
//
// Downloaded, updatedAt and DownloadRate are one sample: they move together or
// not at all.
//
// Only Engine.refresh calls this, on a fixed cadence, so the interval between
// readings *is* the measurement window. Readers must never produce a reading:
// an extra one inserted microseconds after the sampler's consumes the interval
// the next real sample needed and drives displayed rates toward zero.
func (t *Torrent) sample(bytes int64, now time.Time) {
	t.Percent = percent(bytes, t.Size)

	if t.updatedAt.IsZero() {
		// First reading: there is no interval yet, so there is no rate. Leaving
		// it at zero is honest; a rate derived from a zero dt was +Inf, which
		// fails json.Marshal and froze the entire UI.
		t.Downloaded = bytes
		t.updatedAt = now
		return
	}
	dt := now.Sub(t.updatedAt)
	// Arithmetic safety, not debouncing: dt == 0 divides by zero and yields
	// +Inf. Two readings can share a timestamp — the whole pass uses one.
	if dt <= 0 {
		return
	}
	if db := bytes - t.Downloaded; db >= 0 {
		t.DownloadRate = float32(float64(db) * float64(time.Second) / float64(dt))
	} else {
		// Bytes went backwards. The usual cause is Configure re-adding the
		// torrent to a fresh client that has not re-verified yet, so this is the
		// normal path across a rebind rather than an oddity. The old rate is
		// meaningless either way.
		t.DownloadRate = 0
	}
	t.Downloaded = bytes
	t.updatedAt = now
}

// percent returns n/total as a percentage, truncated to two decimals.
//
// Truncated, not rounded, and that is load-bearing: web.newFileView tests
// `Percent >= 100` to decide whether to show a file as complete, so rounding
// would mark a file done at 99.999%.
func percent(n, total int64) float32 {
	if total == 0 {
		return 0
	}
	const scale = 100 // two decimal places
	pct := float64(n) / float64(total) * 100
	return float32(int64(pct*scale)) / scale
}
