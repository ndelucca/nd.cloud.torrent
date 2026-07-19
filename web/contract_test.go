package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// templateSources returns every shipped template's raw text.
func templateSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil || len(entries) == 0 {
		t.Fatalf("no templates found: %v", err)
	}
	out := map[string]string{}
	for _, name := range entries {
		b, err := fs.ReadFile(templateFS, name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = string(b)
	}
	return out
}

var (
	// urlAttrRe matches every attribute whose value a browser resolves as a URL,
	// capturing the name and the raw value.
	//
	// The leading \s is load-bearing: it keeps Alpine expressions (:href) and
	// inert data-* attributes out. DOM-id attributes are excluded by not being
	// named — id="t-{{.InfoHash}}" and aria-controls="tf-{{.InfoHash}}" are ids,
	// where urlpath would be actively wrong, since it would encode the id out of
	// correspondence with the "#tf-…" selector that finds it.
	//
	// The value alternation consumes a whole {{…}} before falling back to [^"],
	// so a quote inside an action — {{if eq .Preview "video"}} — does not end the
	// match early.
	urlAttrRe = regexp.MustCompile(
		`\s(href|src|action|formaction|poster|hx-(?:get|post|put|patch|delete)|sse-connect)="((?:\{\{.*?\}\}|[^"])*)"`)
	// urlAttrNameRe is the same attribute set without the value. Its only job is
	// to count; see TestURLAttributeScanHasNoBlindSpot.
	urlAttrNameRe = regexp.MustCompile(
		`\s(?:href|src|action|formaction|poster|hx-(?:get|post|put|patch|delete)|sse-connect)=`)
	// tmplCommentRe strips {{/* … */}} before scanning. These templates document
	// their htmx wiring in prose, quoting attributes; a comment is not markup.
	tmplCommentRe = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)
	// actionRe is one template action. Non-greedy over any byte rather than
	// "anything but }", so an action containing a brace still terminates at its
	// own delimiter.
	actionRe  = regexp.MustCompile(`\{\{.*?\}\}`)
	sseSwapRe = regexp.MustCompile(`sse-swap="([^"]+)"`)
)

// nonEmitting are the actions that put nothing into the attribute value. An
// {{if}} condition is evaluated, not rendered, and the literal text around it is
// static.
var nonEmitting = map[string]bool{
	"if": true, "else": true, "end": true, "range": true,
	"with": true, "break": true, "continue": true,
}

// urlpathGuarded classifies one action found inside a URL attribute value. It
// reports whether the action is acceptable, and separately whether it emits
// anything at all — the caller needs the second to write a useful message.
//
// {{template …}} is deliberately not in nonEmitting: it emits bytes this
// scanner cannot follow, so it fails rather than passing silently.
func urlpathGuarded(action string) (ok, emits bool) {
	body := strings.Trim(strings.TrimSuffix(strings.TrimPrefix(action, "{{"), "}}"), "- \t")
	fields := strings.Fields(body)
	if len(fields) == 0 || nonEmitting[fields[0]] {
		return true, false
	}
	// The last command in the pipeline is the outermost one, so {{urlpath .P}}
	// and {{.P | urlpath}} both pass while {{urlpath .P | printf "%s#"}} does not.
	segs := strings.Split(body, "|")
	last := strings.Fields(segs[len(segs)-1])
	return len(last) > 0 && last[0] == "urlpath", true
}

// TestTemplateURLsAreDeclared pins the half of the htmx contract that lives in
// this package: every path a template asks the server for is declared in
// KnownRoutes or is a route the server owns.
//
// Nothing tied these together before. A region name, a fragment URL, a DOM id
// and a swap target were string-matched across Go consts, {{define}} names and
// HTML attributes, with no single place listing them and no test asserting that
// a URL in a template is a URL anything answers. The server side of this is
// TestKnownRoutesResolve, which walks KnownRoutes against the real mux.
func TestTemplateURLsAreDeclared(t *testing.T) {
	// Routes owned by the server rather than this package. Listed explicitly so
	// adding one is a decision, not an accident.
	serverOwned := map[string]bool{
		"/api/add":                   true,
		"/api/torrentfile":           true,
		"/api/configure":             true,
		"/api/torrents/{hash}/start": true,
		"/api/torrents/{hash}/stop":  true,
		"/api/torrents/{hash}":       true,
		"/download/{path}":           true,
	}
	known := map[string]bool{}
	for _, r := range KnownRoutes {
		known[r] = true
	}
	// The embedded assets are declared paths too — page.html loads them by name.
	// That they resolve is not enough, so server.TestStaticAssetsAreServed
	// fetches each one; see the StaticAssets doc.
	for _, a := range StaticAssets {
		known[a] = true
	}

	for name, src := range templateSources(t) {
		src = tmplCommentRe.ReplaceAllString(src, "")
		for _, m := range urlAttrRe.FindAllStringSubmatch(src, -1) {
			raw := m[2]
			if !strings.HasPrefix(raw, "/") {
				continue // relative or javascript: — not a server path
			}
			// Normalise the template actions to the pattern wildcards the route
			// table uses, so the two are comparable.
			norm := actionRe.ReplaceAllString(raw, "{hash}")
			if strings.HasPrefix(norm, "/download/") {
				norm = "/download/{path}"
			}
			if !known[norm] && !serverOwned[norm] {
				t.Errorf("%s asks for %q (normalised %q), which is not a declared route",
					name, raw, norm)
			}
		}
	}
}

// TestURLAttributesUseURLPath makes the urlpath contract a gate instead of a
// convention.
//
// The reason is usually told as "html/template does not know hx-delete is a
// URL", which is true and incomplete. html/template's URL *normalizer* — the
// one that does run on href and src — leaves '#', '?' and '&' unescaped on
// purpose, because it is normalizing a URL rather than escaping a path (see
// urlProcessor in html/template/url.go). So a file named "a#b.mkv" truncates to
// "a" in an href exactly as it did in the hx-delete that started this. Every URL
// attribute needs urlpath; none of them are covered by the escaper.
func TestURLAttributesUseURLPath(t *testing.T) {
	for name, src := range templateSources(t) {
		src = tmplCommentRe.ReplaceAllString(src, "")
		for _, m := range urlAttrRe.FindAllStringSubmatch(src, -1) {
			attr, value := m[1], m[2]
			actions := actionRe.FindAllString(value, -1)
			if len(actions) == 0 {
				continue // a constant path: nothing to escape
			}
			if !strings.HasPrefix(value, "/") {
				t.Errorf("%s: %s=%q is templated but is not a rooted path.\n"+
					"urlpath escapes a path, not a whole URL — it would mangle a scheme "+
					"and a query string. A URL built from data needs a template.URL value "+
					"and a reason, not this func.", name, attr, value)
				continue
			}
			for _, a := range actions {
				ok, emits := urlpathGuarded(a)
				if ok {
					continue
				}
				if !emits {
					t.Errorf("%s: %s=%q contains %s, which this check cannot follow.",
						name, attr, value, a)
					continue
				}
				inner := strings.Trim(strings.Trim(a, "{}"), "- \t")
				t.Errorf("%s: %s=%q interpolates %s without urlpath.\n"+
					"Write {{urlpath %s}}. html/template leaves '#' and '?' unescaped in "+
					"href and src, and treats an htmx attribute as plain text, so a value "+
					"containing either silently addresses a different resource and the "+
					"server answers 200.\n"+
					"If this value is a DOM id rather than a path, urlpath is the wrong "+
					"answer and so is the attribute.",
					name, attr, value, a, inner)
			}
		}
	}
}

// TestURLAttributeScanHasNoBlindSpot keeps the scanner above honest.
//
// A test that scans for violations is green both when there are none and when
// it has stopped matching anything, and those look identical. This counts the
// attributes urlAttrRe found against a pattern that only looks for their names:
// they diverge when a value is single-quoted or unquoted, or when an action
// containing a quote defeats the value alternation. Without this, the gate can
// quietly stop being one.
func TestURLAttributeScanHasNoBlindSpot(t *testing.T) {
	for name, src := range templateSources(t) {
		src = tmplCommentRe.ReplaceAllString(src, "")
		got := len(urlAttrRe.FindAllString(src, -1))
		want := len(urlAttrNameRe.FindAllString(src, -1))
		if got != want {
			t.Errorf("%s: matched %d URL attribute values but found %d URL attribute "+
				"names; the value pattern is skipping one. URL attribute values must be "+
				"double-quoted for this scan to see them.", name, got, want)
		}
	}
}

// TestURLPathGuardClassifier exercises urlpathGuarded directly. The template
// corpus passes by construction, so without this the classifier's rejection
// paths are never run.
func TestURLPathGuardClassifier(t *testing.T) {
	cases := []struct {
		action    string
		ok, emits bool
	}{
		{"{{if .Loaded}}", true, false},
		{"{{else}}", true, false},
		{"{{end}}", true, false},
		{"{{range .Files}}", true, false},
		{"{{urlpath .Path}}", true, true},
		{"{{.Path | urlpath}}", true, true},
		{"{{- urlpath .Path -}}", true, true},
		{"{{.Path}}", false, true},
		{"{{.InfoHash}}", false, true},
		{`{{template "x" .}}`, false, true},
		{`{{urlpath .Path | printf "%s#"}}`, false, true},
	}
	for _, c := range cases {
		ok, emits := urlpathGuarded(c.action)
		if ok != c.ok || emits != c.emits {
			t.Errorf("urlpathGuarded(%q) = (%v, %v), want (%v, %v)",
				c.action, ok, emits, c.ok, c.emits)
		}
	}
}

// TestSSESwapNamesAreEmitted pins the other half, in both directions: every
// region a template listens for is one the renderer emits, and every region the
// renderer emits is listened for somewhere. The reverse direction is the one
// that catches a rename applied on only one side.
func TestSSESwapNamesAreEmitted(t *testing.T) {
	emitted := map[string]bool{
		torrentListEvent:      true,
		statsEvent:            true,
		downloadsChangedEvent: true,
	}

	listened := map[string]bool{}
	for name, src := range templateSources(t) {
		for _, m := range sseSwapRe.FindAllStringSubmatch(src, -1) {
			region := m[1]
			// Every region name is now a literal. There used to be a special case
			// here for the infohash-namespaced per-torrent regions, and dropping
			// it makes this check strictly stricter: a templated sse-swap value is
			// no longer excusable, it is a region the renderer cannot be shown to
			// emit.
			listened[region] = true
			if !emitted[region] {
				t.Errorf("%s listens for sse-swap=%q, which the renderer never emits", name, region)
			}
		}
	}

	// hx-trigger carries the ping regions rather than sse-swap.
	for _, src := range templateSources(t) {
		for region := range emitted {
			if strings.Contains(src, "sse:"+region) {
				listened[region] = true
			}
		}
	}

	for region := range emitted {
		if !listened[region] {
			t.Errorf("the renderer emits %q but no template listens for it", region)
		}
	}
}
