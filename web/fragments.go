package web

import (
	"log"
	"net/http"

	"github.com/ndelucca/nd.cloud.torrent/engine"
)

// The fragment handlers answer the hx-get requests for content that is
// deliberately not streamed.
//
// Anything expensive or bulky whose visibility the server cannot know belongs
// here rather than in the SSE stream: per-torrent file tables (only meaningful
// when a row is expanded) and the download tree (changes on the order of
// minutes, while torrent progress changes every second — coupling them to the
// same 1 Hz push is the mistake this avoids).
//
// They take no method guard and do no path parsing of their own: both are the
// routing table's job. A hand-rolled prefix-and-suffix match reads
// "torrent/a/b/files" as the infohash "a/b".

// ServeDownloads renders the download tree.
func (u *UI) ServeDownloads(w http.ResponseWriter, r *http.Request) {
	u.writeTemplate(w, http.StatusOK, "downloads", newDownloadsView(u.deps.Tree()),
		"Could not render downloads.")
}

// ServeTorrentFiles renders one torrent's file table. The infohash comes from
// the route pattern, so it is a single path segment by construction.
func (u *UI) ServeTorrentFiles(w http.ResponseWriter, r *http.Request) {
	t, ok := u.deps.TorrentFiles(r.PathValue("hash"))
	if !ok {
		// A fragment response is HTML, not an error page: htmx swaps whatever
		// comes back straight into the document.
		u.writeTemplate(w, http.StatusNotFound, "fragment-message",
			"Torrent not found.", "Torrent not found.")
		return
	}
	v := newTorrentViewWithFiles(t)
	u.writeTemplate(w, http.StatusOK, "torrent-files", v, "Could not render files.")
}

// ServePage renders the htmx shell. Unlike the fragments it is a full document,
// so it is rendered into a buffer first: a template error halfway through would
// otherwise ship a truncated page with a 200.
func (u *UI) ServePage(w http.ResponseWriter, r *http.Request) {
	view := pageView{Title: u.deps.Title, Config: u.deps.Config()}
	body, err := u.renderer.execute("page", view)
	if err != nil {
		log.Printf("render page: %s", err)
		http.Error(w, "Template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body)
}

// pageView is the htmx shell's model.
type pageView struct {
	Title  string
	Config engine.Config
}

// writeTemplate renders a pulled fragment, falling back to a message rather
// than an error page: htmx swaps whatever comes back straight into the
// document. It goes through renderer.execute, so checkFragment covers the
// pulled fragments too.
//
// The fallback is itself a template, so the markup and its classes stay in the
// template set where the template tests can see them. Only the last resort
// below is a literal.
func (u *UI) writeTemplate(w http.ResponseWriter, status int, name string, data any, fallback string) {
	body, err := u.renderer.execute(name, data)
	if err != nil {
		log.Printf("render %s: %s", name, err)
		u.writeMessage(w, http.StatusInternalServerError, fallback)
		return
	}
	writeFragment(w, status, body)
}

// writeMessage renders a plain message fragment.
func (u *UI) writeMessage(w http.ResponseWriter, status int, msg string) {
	body, err := u.renderer.execute("fragment-message", msg)
	if err != nil {
		// The last resort, and the only HTML literal in this package: the
		// template set itself is broken, so there is nothing left to render
		// with. Kept minimal and classless for that reason.
		log.Printf("render fragment-message: %s", err)
		writeFragment(w, status, []byte("<p>Unavailable.</p>"))
		return
	}
	writeFragment(w, status, body)
}

func writeFragment(w http.ResponseWriter, status int, html []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(html)
}

// WriteAPIResult renders the outcome of an /api/* call as an HTML fragment.
//
// This is the whole of the API layer's dependency on the template set. It takes
// the message to display, not an error: deciding what a failure should say —
// and what it should not say, since operational failures carry syscall strings
// and filesystem paths — is the server's job, in classify.
//
// ok is passed rather than inferred from the message being empty. A successful
// action can have something to say ("saved, but restart to apply it"), and
// inferring would render that in the error style.
func (u *UI) WriteAPIResult(w http.ResponseWriter, msg string, ok bool) {
	name := "api-ok"
	if !ok {
		name = "api-error"
	}
	if msg == "" {
		msg = "Done."
	}
	body, err := u.renderer.execute(name, msg)
	if err != nil {
		log.Printf("render %s: %s", name, err)
		u.writeMessage(w, http.StatusOK, "Unexpected error.")
		return
	}
	writeFragment(w, http.StatusOK, body)
}
