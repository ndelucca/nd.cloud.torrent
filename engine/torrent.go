package engine

import (
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
	Files      []*File
	//cloud torrent
	Started      bool
	Percent      float32
	DownloadRate float32
	//internal — never leaves the engine
	t         *torrent.Torrent
	spec      *torrent.TorrentSpec
	updatedAt time.Time
}

// File is the per-file view model.
type File struct {
	//anacrolix/torrent
	Path      string
	Size      int64
	Chunks    int
	Completed int
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
	c.Files = make([]*File, len(t.Files))
	for i, f := range t.Files {
		if f == nil {
			continue
		}
		cf := *f
		c.Files[i] = &cf
	}
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
	t.Size = tt.Length()

	tfiles := tt.Files()
	t.resizeFiles(len(tfiles))
	for i, f := range tfiles {
		file := t.Files[i]
		if file == nil {
			file = &File{Path: f.Path()}
			t.Files[i] = file
		}
		chunks := f.State()
		completed := 0
		for _, p := range chunks {
			if p.Complete {
				completed++
			}
		}
		file.Size = f.Length()
		file.Chunks = len(chunks)
		file.Completed = completed
		file.Percent = percent(int64(completed), int64(len(chunks)))
	}

	now := time.Now()
	bytes := tt.BytesCompleted()
	t.Percent = percent(bytes, t.Size)
	// Rate is a derivative, so it needs both a previous sample and a non-zero
	// interval — a zero dt used to yield +Inf, which fails json.Marshal and
	// freezes the entire UI.
	if dt := now.Sub(t.updatedAt); !t.updatedAt.IsZero() && dt > 0 {
		if db := bytes - t.Downloaded; db >= 0 {
			t.DownloadRate = float32(float64(db) * float64(time.Second) / float64(dt))
		} else {
			// Bytes went backwards (torrent was re-added); the old rate is
			// meaningless now.
			t.DownloadRate = 0
		}
	}
	t.Downloaded = bytes
	t.updatedAt = now
}

// percent returns n/total as a percentage rounded to two decimals.
func percent(n, total int64) float32 {
	if total == 0 {
		return 0
	}
	const scale = 100 // two decimal places
	pct := float64(n) / float64(total) * 100
	return float32(int64(pct*scale)) / scale
}

// resizeFiles makes the cached file slice match the live file count,
// preserving the entries that survive.
//
// It runs on every pass: the file list is only trustworthy once metadata has
// arrived, and an under-sized slice used to panic on index.
func (t *Torrent) resizeFiles(n int) {
	if len(t.Files) == n {
		return
	}
	resized := make([]*File, n)
	copy(resized, t.Files)
	t.Files = resized
}
