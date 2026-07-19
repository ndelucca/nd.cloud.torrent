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

// File is the per-file progress view. It is a value: a nil entry was only ever
// possible because the slice used to be patched in place, and every consumer
// paid for that with a nil check.
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
func (t *Torrent) update(tt *torrent.Torrent) {
	t.Name = tt.Name()
	t.Loaded = tt.Info() != nil
	if t.Loaded {
		t.updateLoaded(tt)
	}
	t.t = tt
}

func (t *Torrent) updateLoaded(tt *torrent.Torrent) {
	// Rebuilt each pass rather than patched in place. The previous version kept
	// surviving entries by index, which assumed index i is the same file across
	// ticks — untrue once a torrent is re-added, and unwritten anywhere. Every
	// field is read from the live torrent regardless, so preserving nothing
	// costs one small allocation per torrent per tick.
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
	t.sample(tt.BytesCompleted(), time.Now())
}

// minRateInterval is the shortest gap that may be used to compute a rate. The
// poll loop runs at 1s; anything much below that is a second reader, not a new
// measurement.
const minRateInterval = 250 * time.Millisecond

// sample records a progress reading and derives the download rate from it.
//
// Downloaded, updatedAt and DownloadRate are one sample and move together or
// not at all. They used to move apart: every caller advanced Downloaded and
// updatedAt while the rate was only recomputed when the interval happened to be
// positive. Since GetTorrents refreshes on read, an extra reader — /api/state,
// or opening a torrent's Files panel — inserted a reading microseconds after
// the poll loop's, consuming the interval the next real sample needed and
// driving the displayed rate toward zero. Two clients polling once a second
// roughly halved every rate on the page.
//
// A reading that arrives too soon is therefore dropped whole rather than
// half-applied.
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
	if dt < minRateInterval {
		return
	}
	if db := bytes - t.Downloaded; db >= 0 {
		t.DownloadRate = float32(float64(db) * float64(time.Second) / float64(dt))
	} else {
		// Bytes went backwards (the torrent was re-added); the old rate is
		// meaningless now.
		t.DownloadRate = 0
	}
	t.Downloaded = bytes
	t.updatedAt = now
}

// percent returns n/total as a percentage, truncated to two decimals.
//
// Truncated, not rounded, and that is load-bearing: torrents.html tests
// `eq .Percent 100.0` to decide whether to show a file as complete, so rounding
// would mark a file done at 99.999%.
func percent(n, total int64) float32 {
	if total == 0 {
		return 0
	}
	const scale = 100 // two decimal places
	pct := float64(n) / float64(total) * 100
	return float32(int64(pct*scale)) / scale
}
