package web

import (
	"bytes"
	"log"
	"net/http"
	"strings"

	"github.com/ndelucca/nd.cloud.torrent/engine"
	"github.com/ndelucca/nd.cloud.torrent/files"
)

// ServeFragment answers the hx-get requests for content that is deliberately
// not streamed.
//
// Anything expensive or bulky whose visibility the server cannot know belongs
// here rather than in the SSE stream: per-torrent file tables (only meaningful
// when a row is expanded) and the download tree (changes on the order of
// minutes, while torrent progress changes every second — coupling them to the
// same 1 Hz push is the mistake this avoids).
func (u *UI) ServeFragment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/fragments/")

	switch {
	case strings.HasPrefix(rest, "torrent/") && strings.HasSuffix(rest, "/files"):
		hash := strings.TrimSuffix(strings.TrimPrefix(rest, "torrent/"), "/files")
		u.serveTorrentFiles(w, hash)
	case rest == "downloads":
		u.serveDownloadsTree(w)
	default:
		http.Error(w, "Not found", http.StatusNotFound)
	}
}

func (u *UI) serveDownloadsTree(w http.ResponseWriter) {
	root := u.deps.Tree()
	view := struct {
		Root      fsView
		Truncated bool
		Limit     int
	}{
		Root:      newRootView(root),
		Truncated: root.Truncated,
		Limit:     files.Limit,
	}
	var buf bytes.Buffer
	if err := u.renderer.tmpl.ExecuteTemplate(&buf, "downloads", view); err != nil {
		log.Printf("render downloads: %s", err)
		writeFragment(w, http.StatusInternalServerError, `<p class="muted">Could not render downloads.</p>`)
		return
	}
	writeFragment(w, http.StatusOK, buf.String())
}

func (u *UI) serveTorrentFiles(w http.ResponseWriter, hash string) {
	var found *torrentView
	for _, t := range u.deps.Torrents() {
		if t.InfoHash == hash {
			v := newTorrentViewWithFiles(t)
			found = &v
			break
		}
	}
	if found == nil {
		// A fragment response is HTML, not an error page: htmx swaps whatever
		// comes back straight into the document.
		writeFragment(w, http.StatusNotFound, `<p class="muted">Torrent not found.</p>`)
		return
	}

	// Sorted here rather than in the browser: it costs nothing on this side and
	// the client never has to re-sort on every update.
	sortFilesByPath(found.Files)

	var buf bytes.Buffer
	if err := u.renderer.tmpl.ExecuteTemplate(&buf, "torrent-files", found); err != nil {
		log.Printf("render torrent-files: %s", err)
		writeFragment(w, http.StatusInternalServerError, `<p class="muted">Could not render files.</p>`)
		return
	}
	writeFragment(w, http.StatusOK, buf.String())
}

// ServePage renders the htmx shell. Unlike the fragments it is a full document,
// so it is rendered into a buffer first: a template error halfway through would
// otherwise ship a truncated page with a 200.
func (u *UI) ServePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Not allowed", http.StatusMethodNotAllowed)
		return
	}
	view := struct {
		Title  string
		Config engine.Config
	}{
		Title:  u.deps.Title,
		Config: u.deps.Config(),
	}

	var buf bytes.Buffer
	if err := u.renderer.tmpl.ExecuteTemplate(&buf, "page", view); err != nil {
		log.Printf("render page: %s", err)
		http.Error(w, "Template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func writeFragment(w http.ResponseWriter, status int, html string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(html))
}

// WriteAPIResult renders the outcome of an /api/* call as an HTML fragment.
//
// This is the whole of the API layer's dependency on the template set. It takes
// the message to display, not an error: deciding what a failure should say —
// and what it should not say, since operational failures carry syscall strings
// and filesystem paths — is the server's job, in classify. An empty message
// means success.
func (u *UI) WriteAPIResult(w http.ResponseWriter, msg string) {
	name := "api-ok"
	if msg == "" {
		msg = "Done."
	} else {
		name = "api-error"
	}
	var buf bytes.Buffer
	if rerr := u.renderer.tmpl.ExecuteTemplate(&buf, name, msg); rerr != nil {
		log.Printf("render %s: %s", name, rerr)
		writeFragment(w, http.StatusOK, `<p class="err-msg">Unexpected error.</p>`)
		return
	}
	writeFragment(w, http.StatusOK, buf.String())
}
