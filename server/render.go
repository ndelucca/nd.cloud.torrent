package server

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"math"
	"path"
	"sort"
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
		"bytes":    humanBytes,
		"round":    func(f float64) string { return fmt.Sprintf("%.0f", f) },
		"pct":      func(f float32) string { return fmt.Sprintf("%.2f", f) },
		"percent":  percentOf,
		"filename": path.Base,
		"ago":      humanAgo,
		"sub":      func(a, b int64) int64 { return a - b },
	}
}

// humanBytes renders a byte count with a metric prefix, matching the `bytes`
// filter the AngularJS UI used.
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

	mu     sync.Mutex
	body   map[string][]byte // event name -> last rendered body
	framed map[string][]byte // event name -> last body, SSE-framed
}

func newRenderer(t *template.Template) *renderer {
	return &renderer{
		tmpl:   t,
		body:   map[string][]byte{},
		framed: map[string][]byte{},
	}
}

// render executes the named template and returns SSE-framed bytes, or nil if
// the output is byte-identical to the previous render of this event.
func (r *renderer) render(event, tmplName string, data any) ([]byte, error) {
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.body[event]; ok && bytes.Equal(prev, body) {
		return nil
	}
	framed := frameSSE(event, body)
	r.body[event] = bytes.Clone(body)
	r.framed[event] = framed
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
	delete(r.body, event)
	delete(r.framed, event)
	return frameSSE(event, nil)
}

// snapshot returns the current framed body of every region, in a stable order,
// for a newly connected subscriber.
func (r *renderer) snapshot() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.framed))
	for name := range r.framed {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([][]byte, 0, len(names))
	for _, name := range names {
		out = append(out, r.framed[name])
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
