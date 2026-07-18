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

// Handler serves and deletes files under a download root.
//
// **It performs no authorization.** DELETE is destructive and this type will
// happily run it for anyone who reaches it; the caller is responsible for
// gating mutation (the server checks same-origin before delegating). Any new
// mutating method added here inherits that assumption, so do not add one
// without checking the caller still gates it.
//
// The request path is taken as relative to Root, so mount it behind
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
	case http.MethodDelete:
		if err := os.RemoveAll(file); err != nil {
			http.Error(w, "Delete failed", http.StatusInternalServerError)
		}
	default:
		http.Error(w, "Not allowed", http.StatusMethodNotAllowed)
	}
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
