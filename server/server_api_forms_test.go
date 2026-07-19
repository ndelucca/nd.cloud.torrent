package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ndelucca/nd.cloud.torrent/engine"
)

// TestParseConfigCheckboxes covers the one genuinely subtle part of an HTML
// form: an unchecked checkbox submits NOTHING. Without a paired hidden field,
// "off" is indistinguishable from "field not present", and a setting could be
// turned on but never off again.
// mustParseForm turns a form-encoded body into the url.Values parseConfig now
// takes. parseConfig used to re-parse the body from bytes itself, because api()
// drained it before knowing the encoding; routing by path removed that need.
func mustParseForm(t *testing.T, body string) url.Values {
	t.Helper()
	v, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", body, err)
	}
	return v
}

func TestParseConfigCheckboxes(t *testing.T) {
	current := engine.Config{
		DownloadDirectory: "/dl", IncomingPort: 5000,
		AutoStart: true, EnableUpload: true, EnableSeeding: true, DisableEncryption: true,
	}

	// The browser sends the hidden "false" for every box, plus "true" for each
	// box that is checked. The checkbox always follows its hidden field.
	body := "DownloadDirectory=/dl&IncomingPort=5000" +
		"&AutoStart=false&AutoStart=true" + // checked
		"&EnableUpload=false" + // unchecked
		"&EnableSeeding=false" + // unchecked
		"&DisableEncryption=false&DisableEncryption=true" // checked

	got, err := parseConfig(mustParseForm(t, body), current)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AutoStart {
		t.Error("AutoStart should stay on")
	}
	if got.EnableUpload {
		t.Error("EnableUpload should be turned OFF — an unchecked box must be " +
			"able to clear a setting, not just leave it alone")
	}
	if got.EnableSeeding {
		t.Error("EnableSeeding should be turned OFF")
	}
	if !got.DisableEncryption {
		t.Error("DisableEncryption should stay on")
	}
}

// TestParseConfigLeavesAbsentFieldsAlone: a partial form must not zero the
// fields it does not mention.
func TestParseConfigLeavesAbsentFieldsAlone(t *testing.T) {
	current := engine.Config{DownloadDirectory: "/dl", IncomingPort: 5000, AutoStart: true}
	body := "IncomingPort=6000"

	got, err := parseConfig(mustParseForm(t, body), current)
	if err != nil {
		t.Fatal(err)
	}
	if got.IncomingPort != 6000 {
		t.Errorf("IncomingPort = %d, want 6000", got.IncomingPort)
	}
	if got.DownloadDirectory != "/dl" {
		t.Errorf("DownloadDirectory = %q, want it untouched", got.DownloadDirectory)
	}
	if !got.AutoStart {
		t.Error("AutoStart was cleared by a form that never mentioned it")
	}
}

func TestParseConfigRejectsBadInput(t *testing.T) {
	current := engine.Config{DownloadDirectory: "/dl", IncomingPort: 5000}
	for name, body := range map[string]string{
		"empty directory":  "DownloadDirectory=%20%20",
		"non-numeric port": "IncomingPort=abc",
	} {
		if _, err := parseConfig(mustParseForm(t, body), current); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestConfigureRejectsNonForm: the configuration form is the only supported
// encoding. Accepting a second one silently meant two parsers to keep in step
// with the same struct.
//
// The check now lives in handleConfigure rather than parseConfig, so this goes
// through the handler: parseConfig takes already-parsed values and no longer
// has an encoding to reject.
func TestConfigureRejectsNonForm(t *testing.T) {
	s := newTestServer(t)
	before := s.engine.Config()

	body := `{"DownloadDirectory":"/new","IncomingPort":4242}`
	r := httptest.NewRequest(http.MethodPost, "/api/configure", strings.NewReader(body))
	r.Header.Set("Origin", "http://"+r.Host)
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a JSON body", w.Code)
	}
	if got := s.engine.Config(); got != before {
		t.Errorf("a rejected body must leave the config alone, got %+v", got)
	}
}

// TestAddURIDispatch covers the server-side scheme dispatch: one endpoint, and
// the client needs no parsing rules of its own.
func TestAddURIDispatch(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	cases := []struct {
		name, uri string
		wantErr   bool
	}{
		{"magnet", "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20), false},
		{"empty", "", true},
		{"file scheme", "file:///etc/passwd", true},
		{"nonsense", "hello world", true},
	}
	// A live loopback target, so the guard is what rejects it rather than a
	// connection-refused from a closed port.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("SSRF guard failed: /api/add reached a loopback address")
	}))
	defer target.Close()
	cases = append(cases, struct {
		name, uri string
		wantErr   bool
	}{"loopback url", target.URL + "/x.torrent", true})

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/add",
				strings.NewReader("uri="+urlEncode(c.uri)))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			failed := w.Code >= 400
			if failed != c.wantErr {
				t.Errorf("%s: status %d (%q), wantErr=%v", c.uri, w.Code, w.Body.String(), c.wantErr)
			}
		})
	}
}

// TestHTMXGetsHTMLNotPlainText: htmx does not swap non-2xx by default, so the
// outcome comes back as a 200 fragment. Every other client keeps real status
// codes.
func TestHTMXGetsHTMLNotPlainText(t *testing.T) {
	s := newTestServer(t)
	h := s.handler()

	// Same failing request, with and without the htmx header.
	path := "/api/torrents/" + strings.Repeat("ab", 20) + "/start"

	plain := httptest.NewRequest(http.MethodPost, path, nil)
	pw := httptest.NewRecorder()
	h.ServeHTTP(pw, plain)
	if pw.Code != http.StatusNotFound {
		t.Errorf("non-htmx status = %d, want 404 (status codes must survive)", pw.Code)
	}
	if ct := pw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("non-htmx Content-Type = %q, want text/plain", ct)
	}

	hx := httptest.NewRequest(http.MethodPost, path, nil)
	hx.Header.Set("HX-Request", "true")
	hw := httptest.NewRecorder()
	h.ServeHTTP(hw, hx)
	if hw.Code != http.StatusOK {
		t.Errorf("htmx status = %d, want 200 so the fragment is swapped", hw.Code)
	}
	if ct := hw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("htmx Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(hw.Body.String(), "err-msg") {
		t.Errorf("htmx body should carry the error fragment, got %q", hw.Body.String())
	}
}

func urlEncode(s string) string {
	r := strings.NewReplacer(
		"%", "%25", "&", "%26", "=", "%3D", "?", "%3F",
		":", "%3A", "/", "%2F", " ", "+", "+", "%2B",
	)
	return r.Replace(s)
}
