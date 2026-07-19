package configfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ndelucca/nd.cloud.torrent/engine"
)

// These tests used to live in the server package, where asserting that a JSON
// file is written atomically cost an engine, a template parse, a bound UDP port
// and two free TCP ports. Here a filesystem-durability guarantee is tested as a
// filesystem-durability guarantee.

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	c := Defaults()
	c.EnableSeeding = true
	if err := Save(path, c); err != nil {
		t.Fatalf("Save: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got engine.Config
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("saved config does not parse: %v\n%s", err, b)
	}
	if !got.EnableSeeding {
		t.Fatal("saved config lost the change")
	}

	// The temp file is a sibling, so a leak would be visible here — and would
	// accumulate one file per save.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".cloud-torrent-") {
			t.Fatalf("temp file %s was left behind", e.Name())
		}
	}
}

// TestSaveCreatesParentDirectory covers --config-path pointing somewhere that
// does not exist yet, which used to fail the save outright.
func TestSaveCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "config.json")
	if err := Save(path, Defaults()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config was not written: %v", err)
	}
}

func TestSaveOverwritesInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	first := Defaults()
	first.DownloadDirectory = "/one"
	if err := Save(path, first); err != nil {
		t.Fatal(err)
	}
	second := Defaults()
	second.DownloadDirectory = "/two"
	if err := Save(path, second); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.DownloadDirectory != "/two" {
		t.Errorf("DownloadDirectory = %q, want the second save to win", got.DownloadDirectory)
	}
}

// Load's branches had no direct test before: only one server-level test reached
// it, and only through New.
func TestLoad(t *testing.T) {
	t.Run("absent file yields defaults", func(t *testing.T) {
		got, err := Load(filepath.Join(t.TempDir(), "nope.json"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got != Defaults() {
			t.Errorf("Load = %+v, want the defaults", got)
		}
	})

	t.Run("empty file yields defaults", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		got, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got != Defaults() {
			t.Errorf("Load = %+v, want the defaults", got)
		}
	})

	t.Run("malformed json is an error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatal("malformed JSON must be reported, not silently defaulted")
		} else if !strings.Contains(err.Error(), "malformed configuration") {
			t.Errorf("error = %v, want it to name the problem", err)
		}
	})

	// The no-clamp contract, previously observable only from a running server:
	// a partial file keeps the defaults for what it does not mention, and keeps
	// an explicitly written value even when that value is invalid — rejecting it
	// is engine.Config.Validate's job, not this package's.
	t.Run("partial json keeps defaults for absent fields", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"DownloadDirectory":"/custom"}`), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.DownloadDirectory != "/custom" {
			t.Errorf("DownloadDirectory = %q, want /custom", got.DownloadDirectory)
		}
		if got.IncomingPort != defaultIncomingPort {
			t.Errorf("IncomingPort = %d, want the default %d", got.IncomingPort, defaultIncomingPort)
		}
		if !got.AutoStart || !got.EnableUpload {
			t.Error("absent booleans must keep their defaults, not zero out")
		}
	})

	t.Run("an out-of-range port survives Load", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"IncomingPort":99999}`), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.IncomingPort != 99999 {
			t.Errorf("IncomingPort = %d: Load must not clamp. Validate rejects it "+
				"and reports why; clamping here would make two policies for one rule",
				got.IncomingPort)
		}
		if err := got.Validate(); err == nil {
			t.Error("Validate must reject the port Load faithfully preserved")
		}
	})
}

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	want := engine.Config{
		AutoStart:         true,
		DisableEncryption: true,
		DownloadDirectory: "/downloads",
		EnableUpload:      false,
		EnableSeeding:     true,
		IncomingPort:      12345,
	}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
	}
}
