package engine

import (
	"context"
	"slices"
	"time"

	"github.com/anacrolix/torrent"
)

// Torrent is one torrent's progress: the wire contract of GET /api/state and
// the source for the streamed row in the UI. Exported field names are part of
// both contracts.
//
// It deliberately carries no file list. The streamed row never renders one, and
// GetTorrents runs once per sample for every connected browser — copying every
// file into every row once a second is pure waste. Callers that need files ask
// for one torrent by key.
type Torrent struct {
	//anacrolix/torrent
	InfoHash   string
	Name       string
	Loaded     bool
	Downloaded int64
	Size       int64
	//cloud torrent
	Started      bool
	Percent      float32
	DownloadRate float32
}

// TorrentWithFiles is a Torrent plus its file table, for the on-demand file
// fragment and for /api/state.
type TorrentWithFiles struct {
	Torrent
	Files []File
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

// torrentState is the engine's internal record for one torrent. It never leaves
// the package: the live client handle and the spec are how the engine acts on a
// torrent, and handing them out would let a caller reach the underlying client.
type torrentState struct {
	Torrent
	Files     []File
	t         *torrent.Torrent
	spec      *torrent.TorrentSpec
	updatedAt time.Time

	// watching is the handle the live metadata watcher is parked on, and
	// cancelWatch releases it. Both are nil when no watcher is live.
	//
	// The pair exists because a watcher outlives the handle it waits on:
	// Drop does not close GotInfo, so without a cancel the watcher for an
	// unresolvable magnet parks until the engine closes, and add/delete churn
	// accumulates goroutines for as long as the process runs.
	watching    *torrent.Torrent
	cancelWatch context.CancelFunc
}

// stopWatch releases the metadata watcher, if one is live. Idempotent. Callers
// must hold the engine mutex.
func (t *torrentState) stopWatch() {
	if t.cancelWatch != nil {
		t.cancelWatch()
		t.cancelWatch = nil
	}
	t.watching = nil
}

// view returns the progress snapshot. No file list and no internal handles —
// both are unrepresentable in Torrent rather than nil'd out by hand.
func (t *torrentState) view() *Torrent {
	c := t.Torrent
	return &c
}

// viewWithFiles is view plus a copy of the file table. slices.Clone is the whole
// of the deep copy: File is a value type.
func (t *torrentState) viewWithFiles() *TorrentWithFiles {
	return &TorrentWithFiles{Torrent: t.Torrent, Files: slices.Clone(t.Files)}
}

// update copies live state out of the underlying torrent. Callers must hold the
// engine mutex.
//
// now is passed in rather than read here so one pass shares a single timestamp,
// and so a test can drive a deterministic two-sample sequence without a real
// clock or a real download.
func (t *torrentState) update(tt *torrent.Torrent, now time.Time) {
	t.Name = tt.Name()
	t.Loaded = tt.Info() != nil
	if t.Loaded {
		t.updateLoaded(tt, now)
	}
	t.t = tt
}

func (t *torrentState) updateLoaded(tt *torrent.Torrent, now time.Time) {
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
func (t *torrentState) sample(bytes int64, now time.Time) {
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
