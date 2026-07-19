package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

// parseTemplates loads every fragment. Templates are addressed exclusively by
// their {{define}} name, never by filename: template.ParseFS names templates by
// base name, so two files called row.html in different directories would
// silently collide, and asking for a name no file provides yields an *empty*
// template that renders nothing without erroring.
//
// The error is returned rather than panicked because the recursive fsnode
// template can fail html/template's contextual autoescaper with
// ErrOutputContext at parse time; a package-level Must would turn a template
// edit into a startup panic.
func parseTemplates() (*template.Template, error) {
	t, err := template.New("cloud-torrent").Funcs(templateFuncs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return t, nil
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"bytes":   humanBytes,
		"round":   func(f float64) string { return fmt.Sprintf("%.0f", f) },
		"pct":     func(f float32) string { return fmt.Sprintf("%.2f", f) },
		"ago":     humanAgo,
		"urlpath": urlPath,
	}
}

// urlPath percent-encodes each segment of a slash-separated path for use in a
// URL.
//
// html/template only normalizes attributes it recognises as URLs — href, src
// and friends. An htmx attribute like hx-delete is just text to it, so a file
// named "a#b.mkv" produced hx-delete="/download/a#b.mkv"; the browser dropped
// everything from the "#" and the server deleted a *different* file named "a",
// answering 200. File names come from torrents, so that was attacker-reachable.
// "?" truncated the same way and "%" made the request fail outright.
//
// Segments are escaped individually so the separators survive. PathEscape
// leaves "/" alone, which is why splitting first is necessary.
func urlPath(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}

// humanBytes renders a byte count with a metric prefix.
func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	e := int(math.Floor(math.Log(float64(n)) / math.Log(1000)))
	if e >= len(units) {
		e = len(units) - 1
	}
	v := float64(n) / math.Pow(1000, float64(e))
	if e == 0 {
		return fmt.Sprintf("%.0f %s", v, units[e])
	}
	return fmt.Sprintf("%.1f %s", v, units[e])
}

// percentOf guards the division by zero that used to produce +Inf and break
// JSON marshalling of the whole state document.
func percentOf(n, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

// renderer owns the parsed templates and the per-region cache of the last bytes
// rendered for each SSE event.
//
// Change detection compares *rendered output*, not source data. Comparing
// source data would mean maintaining an Equal method per view model and
// remembering to update it whenever a template changes; the failure mode there
// is a silently stale UI. Comparing output is self-maintaining.
//
// The rendered bytes are retained rather than hashed because a client that
// connects mid-tick must immediately receive the current body of every region
// rather than wait for the next change. Given they are retained anyway,
// bytes.Equal is as cheap as hashing and has no collision question.
type renderer struct {
	tmpl *template.Template

	// One map, not two. Framing is a pure function of (event, body) and the
	// event is the key, so a separate body map carried no information the
	// framed one did not — it was two things to keep in step for nothing.
	mu         sync.Mutex
	framedBody map[string][]byte // event name -> last body, SSE-framed
}

func newRenderer(t *template.Template) *renderer {
	return &renderer{
		tmpl:       t,
		framedBody: map[string][]byte{},
	}
}

// execute runs a template and returns its body, trimmed and checked.
//
// It is the only place a template is executed. ServePage and the pulled
// fragments used to reach through to r.tmpl.ExecuteTemplate directly, which
// meant checkFragment did not run for them: a bare-text downloads or
// torrent-files fragment would have shipped with no error anywhere, and only
// went unnoticed because those two use innerHTML rather than a morph swap.
func (r *renderer) execute(tmplName string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := r.tmpl.ExecuteTemplate(&buf, tmplName, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", tmplName, err)
	}
	// Trim before framing. A {{define}} block starts with a newline, which would
	// otherwise put a stray text node ahead of the element and cost a "data: "
	// line per blank line on every push.
	body := bytes.TrimSpace(buf.Bytes())
	if err := checkFragment(tmplName, body); err != nil {
		return nil, err
	}
	return body, nil
}

// render executes the named template and returns SSE-framed bytes, or nil if
// the output is byte-identical to the previous render of this event.
func (r *renderer) render(event, tmplName string, data any) ([]byte, error) {
	body, err := r.execute(tmplName, data)
	if err != nil {
		return nil, err
	}
	return r.store(event, body), nil
}

// errBareText rejects a fragment that is not wrapped in an element.
var errBareText = errors.New("fragment must be wrapped in an element")

// checkFragment enforces the one rule idiomorph does not enforce for us.
//
// Verified against Chromium 150 with htmx 2.0.10 + idiomorph 0.7.4: a payload
// of bare text swapped with hx-swap="morph:…" produces an EMPTY target. No
// console error, no htmx event, the data: line arrives intact — the DOM is just
// blank. (A plain innerHTML swap of the same payload works, which is what makes
// it so easy to miss.) Failing loudly at render time is much cheaper than
// finding it in a browser.
func checkFragment(name string, body []byte) error {
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] != '<' {
		return fmt.Errorf("template %q: %w (starts with %q)", name, errBareText,
			string(t[:min(20, len(t))]))
	}
	return nil
}

// store caches body under event and returns the framed bytes, or nil if
// unchanged.
func (r *renderer) store(event string, body []byte) []byte {
	framed := frameSSE(event, body)
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.framedBody[event]; ok && bytes.Equal(prev, framed) {
		return nil
	}
	r.framedBody[event] = framed
	return framed
}

// forget drops a region's cache and returns a framed empty event.
//
// The empty event is not cosmetic: htmx's SSE extension unregisters a
// per-element listener lazily, from inside the listener itself, when it notices
// the element has left the document. If the server simply stops emitting an
// event name, that listener never runs again, never unregisters, and retains
// the detached DOM subtree forever. One final empty event lets it collect
// itself.
func (r *renderer) forget(event string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.framedBody, event)
	return frameSSE(event, nil)
}

// framed returns the cached framing for a region, for callers that must emit it
// even though its bytes did not change.
func (r *renderer) framed(event string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.framedBody[event]
}

// snapshot returns every region's current framed body as one buffer, for a
// newly connected subscriber, with first's region ahead of the rest.
//
// The ordering is explicit because it is load-bearing and used to be accidental.
// A membership region has to arrive before the item regions it creates
// elements for — an element cannot listen for torrent-<hash> before it exists,
// so those frames are silently discarded. It happened to work only because
// infohashes are lowercase hex and 'l' sorts after 'f', which inverts the day
// anything changes how hashes are encoded.
//
// One buffer rather than a slice of frames: SSE frames are self-delimiting, so
// the caller can make a single Write and a single Flush instead of one syscall
// pair per region.
func (r *renderer) snapshot(first string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.framedBody))
	for name := range r.framedBody {
		if name != first {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var out []byte
	if frame, ok := r.framedBody[first]; ok {
		out = append(out, frame...)
	}
	for _, name := range names {
		out = append(out, r.framedBody[name]...)
	}
	return out
}

// frameSSE wraps a body in SSE framing. Every line of the payload needs its own
// "data: " prefix, so rendered HTML — which is full of newlines — cannot be
// written as a single data line.
func frameSSE(event string, body []byte) []byte {
	var b bytes.Buffer
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteByte('\n')
	if len(body) == 0 {
		// An empty data line still delivers the event, which is what makes the
		// listener-cleanup trick in forget work.
		b.WriteString("data:\n")
	} else {
		body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
		for _, line := range bytes.Split(body, []byte("\n")) {
			b.WriteString("data: ")
			b.Write(line)
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
	return b.Bytes()
}
