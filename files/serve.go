package files

import (
	"archive/zip"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Handler serves files under a download root. It is read-only: the mutating
// operation lives in Remove, which is a plain function so that mounting this
// handler cannot expose one by accident.
//
// **It performs no authorization**, so the caller decides who reaches it. The
// request path is taken as relative to Root, so mount it behind
// http.StripPrefix.
type Handler struct {
	// Root is a func, not a string: /api/configure can move the download
	// directory at any time, and a handler holding a stale copy would serve
	// from the old one.
	Root func() string
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Error text is deliberately generic: echoing the resolved path back turned
	// every rejected probe into a filesystem-layout oracle.
	file, err := ResolveWithin(h.Root(), strings.TrimPrefix(r.URL.Path, "/"))
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	info, err := os.Stat(file)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		sandbox(w)
		if info.IsDir() {
			serveZip(w, r, file, info.Name())
			return
		}
		f, err := os.Open(file)
		if err != nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		defer f.Close()
		http.ServeContent(w, r, info.Name(), info.ModTime(), f)
	default:
		http.Error(w, "Not allowed", http.StatusMethodNotAllowed)
	}
}

// Remove deletes the entry at rel, which must resolve inside root. It is
// recursive: a torrent's download is usually a directory.
//
// A function rather than a method on Handler. Handler is mounted with no
// authorization of its own, so keeping the only destructive operation off it
// means a future mount cannot expose one by accident. **The caller gates this**
// — the server rejects any non-GET that is not same-origin before it is reached.
//
// The containment rule stays here rather than at the call site because it is the
// only thing between a user-supplied path and the rest of the filesystem.
func Remove(root, rel string) error {
	file, err := ResolveWithin(root, rel)
	if err != nil {
		return err
	}
	return os.RemoveAll(file)
}

// sandbox puts a downloaded file in an opaque origin.
//
// Content-Type is derived from the file extension, so a torrent containing an
// index.html is served as text/html from the app's own origin — and nosniff does
// not help when the declared type *is* text/html. Without this, that script runs
// same-origin and can drive every /api/* mutation and DELETE /download/*.
//
// The header lives here rather than in the server's middleware so it travels
// with the bytes and cannot be lost by a future mount. It is ignored for
// non-document responses, so image, audio and video previews are unaffected.
func sandbox(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "sandbox")
}

// serveZip streams a directory as a zip archive. The status is committed as soon
// as the first byte is written, so mid-stream failures can only be logged — but
// the archive is at least closed properly so the client sees a truncated file
// rather than a silently valid-looking one.
func serveZip(w http.ResponseWriter, r *http.Request, dir, name string) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "")+`.zip"`)
	if r.Method == http.MethodHead {
		return
	}
	zw := zip.NewWriter(w)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// A zip of a large download directory reads the whole tree. Without
		// this, a client that navigated away kept the walk and its file reads
		// running to completion with nowhere to write them.
		if ctxErr := r.Context().Err(); ctxErr != nil {
			return ctxErr
		}
		// The same visibility rule the listing walk uses, and applied the same
		// way: to entries, never to the root. Without it the archive carried
		// dotfiles the UI says are not there — the tree and its zip must agree
		// on what exists. Testing the root instead would make a directory the
		// user explicitly asked for zip up empty.
		if path != dir {
			info, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			if !visible(info) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		entry, err := zw.Create(filepath.ToSlash(filepath.Join(name, rel)))
		if err != nil {
			return err
		}
		_, err = io.Copy(entry, f)
		return err
	})
	if err != nil {
		log.Printf("zip %s: %s", dir, err)
	}
	if err := zw.Close(); err != nil {
		log.Printf("zip close %s: %s", dir, err)
	}
}
