package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func newApp() *App { return New("nd-cloud-torrent", "1.2.3", "https://example.test/repo") }

// TestPrecedence pins default → env → flag. Getting this backwards makes an
// explicit flag silently lose to a stale environment variable.
func TestPrecedence(t *testing.T) {
	t.Run("default survives when nothing is set", func(t *testing.T) {
		port := 3000
		a := newApp()
		a.Int(&port, "port", "p", "PORT", "Listening port")
		if err := a.Parse(nil); err != nil {
			t.Fatal(err)
		}
		if port != 3000 {
			t.Errorf("port = %d, want 3000", port)
		}
	})

	t.Run("env overrides default", func(t *testing.T) {
		t.Setenv("PORT", "4000")
		port := 3000
		a := newApp()
		a.Int(&port, "port", "p", "PORT", "Listening port")
		if err := a.Parse(nil); err != nil {
			t.Fatal(err)
		}
		if port != 4000 {
			t.Errorf("port = %d, want 4000 from env", port)
		}
	})

	t.Run("flag overrides env", func(t *testing.T) {
		t.Setenv("PORT", "4000")
		port := 3000
		a := newApp()
		a.Int(&port, "port", "p", "PORT", "Listening port")
		if err := a.Parse([]string{"--port", "5000"}); err != nil {
			t.Fatal(err)
		}
		if port != 5000 {
			t.Errorf("port = %d, want 5000 from the flag", port)
		}
	})

	t.Run("empty env is ignored", func(t *testing.T) {
		t.Setenv("TITLE", "")
		title := "Cloud Torrent"
		a := newApp()
		a.String(&title, "title", "t", "TITLE", "Title")
		if err := a.Parse(nil); err != nil {
			t.Fatal(err)
		}
		if title != "Cloud Torrent" {
			t.Errorf("title = %q; an empty env var must not blank the default", title)
		}
	})
}

// TestShortAndLongAgree covers the alias registration: both names write the same
// target.
func TestShortAndLongAgree(t *testing.T) {
	for _, arg := range []string{"--title", "-title", "-t"} {
		title := "default"
		a := newApp()
		a.String(&title, "title", "t", "TITLE", "Title")
		if err := a.Parse([]string{arg, "set"}); err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		if title != "set" {
			t.Errorf("%s did not apply: title = %q", arg, title)
		}
	}
}

func TestBoolFlag(t *testing.T) {
	verbose := false
	a := newApp()
	a.Bool(&verbose, "log", "l", "", "Enable request logging")
	if err := a.Parse([]string{"-l"}); err != nil {
		t.Fatal(err)
	}
	if !verbose {
		t.Error("-l did not set the bool")
	}
}

// TestHelp checks that -h is help. It used to be --host, which is exactly the
// kind of trap this rewrite was meant to remove.
func TestHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		var buf bytes.Buffer
		title := "Cloud Torrent"
		a := newApp()
		a.out = &buf
		a.String(&title, "title", "t", "TITLE", "Title of this instance")

		if err := a.Parse([]string{arg}); !errors.Is(err, ErrHelp) {
			t.Fatalf("%s returned %v, want ErrHelp", arg, err)
		}
		out := buf.String()
		for _, want := range []string{
			"Usage: nd-cloud-torrent [options]",
			"--title, -t",
			"Title of this instance (default Cloud Torrent, env TITLE)",
			"--help, -h",
			"--version, -v",
			"https://example.test/repo",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s output missing %q\n---\n%s", arg, want, out)
			}
		}
	}
}

func TestVersion(t *testing.T) {
	var buf bytes.Buffer
	a := newApp()
	a.out = &buf
	if err := a.Parse([]string{"--version"}); !errors.Is(err, ErrHelp) {
		t.Fatalf("--version returned %v, want ErrHelp", err)
	}
	if strings.TrimSpace(buf.String()) != "1.2.3" {
		t.Errorf("version output = %q", buf.String())
	}
}

func TestUnknownFlagAndStrayArgFail(t *testing.T) {
	a := newApp()
	if err := a.Parse([]string{"--nope"}); err == nil {
		t.Error("unknown flag accepted")
	}

	b := newApp()
	if err := b.Parse([]string{"leftover"}); err == nil {
		t.Error("stray positional argument accepted")
	}
}

// TestBadEnvValueIsReported: a non-numeric PORT should fail loudly rather than
// silently falling back to the default.
func TestBadEnvValueIsReported(t *testing.T) {
	t.Setenv("PORT", "not-a-number")
	port := 3000
	a := newApp()
	a.Int(&port, "port", "p", "PORT", "Listening port")
	err := a.Parse(nil)
	if err == nil {
		t.Fatal("invalid env value accepted")
	}
	if !strings.Contains(err.Error(), "PORT") {
		t.Errorf("error %q does not name the offending variable", err)
	}
}

// TestBoolHelpHasNoDefault: "(default false)" on a flag whose absence means
// false is noise.
func TestBoolHelpHasNoDefault(t *testing.T) {
	var buf bytes.Buffer
	open := false
	a := newApp()
	a.out = &buf
	// The help text deliberately contains the word "default" itself, so this
	// looks for the rendered "(default …)" suffix, not the bare word.
	a.Bool(&open, "open", "o", "", "Open now with your default browser")
	_ = a.Parse([]string{"--help"})
	if strings.Contains(buf.String(), "(default") {
		t.Errorf("bool flag rendered a default:\n%s", buf.String())
	}
}
