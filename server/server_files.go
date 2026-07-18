package server

import (
	"archive/zip"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fileNumberLimit caps how many entries the download tree will walk.
const fileNumberLimit = 1000

type fsNode struct {
	Name     string
	Size     int64
	Modified time.Time
	Children []*fsNode
	// Truncated marks a tree that hit fileNumberLimit, so the UI can say so
	// instead of presenting a partial listing as complete.
	Truncated bool `json:",omitempty"`
}

func (s *Server) downloadDir() string {
	var dir string
	s.state.Read(func(st *State) { dir = st.Config.DownloadDirectory })
	return dir
}

func (s *Server) listFiles() *fsNode {
	rootDir := s.downloadDir()
	root := &fsNode{}
	info, err := os.Stat(rootDir)
	if err != nil {
		return root
	}
	n := 0
	if err := list(rootDir, info, root, &n); err != nil {
		if errors.Is(err, errFileLimit) {
			root.Truncated = true
		} else {
			log.Printf("File listing failed: %s", err)
		}
	}
	return root
}

var (
	errFileLimit  = errors.New("over file limit")
	errSkipEntry  = errors.New("skip entry")
	errOutsideDir = errors.New("Nice try")
)

// list walks path into node. It returns errFileLimit once the budget is spent,
// which aborts the whole walk and marks the result truncated.
func list(path string, info os.FileInfo, node *fsNode, n *int) error {
	if (!info.IsDir() && !info.Mode().IsRegular()) || strings.HasPrefix(info.Name(), ".") {
		return errSkipEntry
	}
	*n++
	if *n > fileNumberLimit {
		return errFileLimit
	}
	node.Name = info.Name()
	node.Size = info.Size()
	node.Modified = info.ModTime()
	if !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	node.Size = 0
	for _, e := range entries {
		ei, err := e.Info()
		if err != nil {
			continue
		}
		c := &fsNode{}
		if err := list(filepath.Join(path, e.Name()), ei, c, n); err != nil {
			if errors.Is(err, errFileLimit) {
				return err // propagate: the walk is over
			}
			continue // skipped entry or unreadable child
		}
		node.Size += c.Size
		node.Children = append(node.Children, c)
	}
	return nil
}

// resolveWithin maps a user-supplied relative path to an absolute path proven to
// live inside root. A plain strings.HasPrefix check is not enough: it has no
// separator boundary, so "<root>-backup/secret" passes it.
func resolveWithin(root, rel string) (string, error) {
	if root == "" {
		return "", errOutsideDir
	}
	target := filepath.Join(root, filepath.FromSlash(rel))
	if !isWithin(root, target) {
		return "", errOutsideDir
	}
	// Follow symlinks and re-check: a link inside the download directory can
	// otherwise point anywhere on disk.
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if !isWithin(resolvedRoot, resolvedTarget) {
		return "", errOutsideDir
	}
	return resolvedTarget, nil
}

// isWithin reports whether target is strictly inside root.
func isWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return false // the root itself is not a valid target
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/download/")
	// Error text is deliberately generic: echoing the resolved path back turned
	// every rejected probe into a filesystem-layout oracle.
	file, err := resolveWithin(s.downloadDir(), rel)
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
		if err := checkSameOrigin(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
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
