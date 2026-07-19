package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/ndelucca/nd.cloud.torrent/files"
)

// maxAPIBody caps request bodies. A .torrent file is comfortably under this.
const maxAPIBody = 4 << 20 // 4 MiB

// finishAPI turns a handler's error into a response: it wakes the render loop,
// maps the error and picks the representation.
func (s *Server) finishAPI(w http.ResponseWriter, r *http.Request, err error) {
	// Unconditional, including on failure: an action can apply partially and
	// still report an error. Uploading five torrents where two are malformed
	// returns 400 while three are already in the engine, and gating the kick on
	// success leaves those invisible until the next tick. kick is coalesced and
	// floored, so an unnecessary one costs at most a single extra render.
	s.kick()

	status, msg := http.StatusOK, ""
	if err != nil {
		status, msg = classify(err)
		// The user gets a fixed message for anything they did not cause, so the
		// chain is only recoverable from here.
		if status >= http.StatusInternalServerError {
			log.Printf("api %s: %s", r.URL.Path, err)
		}
	}

	// htmx wants HTML to swap. It also does not swap non-2xx responses by
	// default, so the outcome is reported as a 200 fragment; the status codes
	// stay intact for every other client.
	if r.Header.Get("HX-Request") == "true" {
		s.ui.WriteAPIResult(w, msg)
		return
	}

	if err != nil {
		http.Error(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

// readBody drains a request body under the size cap.
func readBody(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxAPIBody))
	if err != nil {
		return nil, badRequest("failed to read request body")
	}
	return data, nil
}

// handleAdd takes a magnet link or an http(s) URL to a .torrent. One field; the
// server dispatches on the scheme, so the client needs no parsing rules of its
// own.
func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) error {
	defer r.Body.Close()
	// The omni form posts form-encoded; scripts post the bare string.
	if isForm(r) {
		if err := r.ParseForm(); err != nil {
			return badRequest("malformed form body")
		}
		return s.addURI(r, strings.TrimSpace(r.PostFormValue("uri")))
	}
	data, err := readBody(r)
	if err != nil {
		return err
	}
	return s.addURI(r, strings.TrimSpace(string(data)))
}

// handleTorrentFile takes raw .torrent bytes, or a multipart upload from the
// browser.
func (s *Server) handleTorrentFile(w http.ResponseWriter, r *http.Request) error {
	defer r.Body.Close()
	// Multipart is checked before the body is drained: it is the only encoding
	// that must be parsed by the stdlib rather than read whole.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return s.addUploadedTorrents(w, r)
	}
	data, err := readBody(r)
	if err != nil {
		return err
	}
	return s.engine.NewTorrentFile(data)
}

// handleConfigure applies the settings form.
func (s *Server) handleConfigure(w http.ResponseWriter, r *http.Request) error {
	defer r.Body.Close()
	if !isForm(r) {
		return badRequest("expected a form-encoded configuration")
	}
	if err := r.ParseForm(); err != nil {
		return badRequest("malformed form body")
	}
	// Read, merge and apply under one lock. engine.configureMu serializes the
	// apply, but not the read the merge is based on, so two concurrent saves
	// could each start from the same config and the second would silently undo
	// the first.
	s.configMu.Lock()
	defer s.configMu.Unlock()
	c, err := parseConfig(r.PostForm, s.engine.Config())
	if err != nil {
		return err
	}
	return s.reconfigure(c)
}

// isForm reports whether the body is url-encoded form data.
func isForm(r *http.Request) bool {
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	return strings.TrimSpace(ct) == "application/x-www-form-urlencoded"
}

// apiHandler is what an /api/* route does. It takes the ResponseWriter because
// the multipart path needs it, for http.MaxBytesReader.
type apiHandler func(http.ResponseWriter, *http.Request) error

// apiRoute adapts an apiHandler into an http.Handler, applying the kick, the
// htmx fragment rendering and the status mapping in exactly one place.
func (s *Server) apiRoute(h apiHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.finishAPI(w, r, h(w, r))
	})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) error {
	return s.engine.StartTorrent(r.PathValue("hash"))
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) error {
	return s.engine.StopTorrent(r.PathValue("hash"))
}

// handleDeleteFile removes a download. The path is the {path...} remainder, so
// it arrives already decoded; files.Remove is what proves it stays inside the
// download directory.
func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) error {
	return files.Remove(s.downloadDir(), r.PathValue("path"))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) error {
	return s.engine.DeleteTorrent(r.PathValue("hash"))
}
