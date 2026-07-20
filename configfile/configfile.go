// Package configfile loads and atomically persists the engine configuration.
//
// It is named for the file, not the type: the config itself lives in the engine
// and nowhere else, and a package called "config" would invite this one to grow
// a second copy of it. There is no state here — three functions over a path.
package configfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ndelucca/nd.cloud.torrent/engine"
)

// defaultIncomingPort is the shipped BitTorrent listen port.
const defaultIncomingPort = 50007

// Defaults returns the configuration a server starts from when no file exists.
func Defaults() engine.Config {
	return engine.Config{
		DownloadDirectory: "./downloads",
		EnableUpload:      true,
		AutoStart:         true,
		IncomingPort:      defaultIncomingPort,
	}
}

// Load reads path over the defaults. A missing or empty file yields the
// defaults; malformed JSON is an error.
func Load(path string) (engine.Config, error) {
	c := Defaults()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read configuration error: %w", err)
	}
	if len(b) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("malformed configuration: %w", err)
	}
	// The port is deliberately not clamped. c starts from Defaults above, so an
	// absent IncomingPort already keeps defaultIncomingPort — a clamp could only
	// fire on a value someone explicitly wrote, and silently rewriting that is
	// worse than reporting it. Port validity is engine.Config.Validate's call and
	// nowhere else; two policies for one rule end up disagreeing.
	return c, nil
}

// Save persists c to path atomically.
func Save(path string, c engine.Config) error {
	b, err := json.MarshalIndent(&c, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode configuration: %w", err)
	}
	if err := writeAtomic(path, b, 0600); err != nil {
		return fmt.Errorf("failed to save configuration: %w", err)
	}
	return nil
}

// writeAtomic writes b to path via a temp file and a rename, so the file is
// either the old contents or the new ones and never a fragment.
//
// An interrupted write-in-place — a crash, a full disk, a container stop —
// leaves a truncated file that Load then rejects as malformed, and the server
// refuses to start until someone deletes it by hand.
//
// It returns raw errors; Save wraps them once. Wrapping at each step here was
// six identical fmt.Errorf calls saying the same thing.
func writeAtomic(path string, b []byte, perm os.FileMode) error {
	// filepath.Dir never returns "", so this needs no guard: a bare filename
	// yields ".", which MkdirAll accepts.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// The temp file goes in the target's directory: rename is only atomic within
	// a filesystem.
	tmp, err := os.CreateTemp(dir, ".cloud-torrent-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds

	// CreateTemp already makes it 0600, but say so rather than rely on it.
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	// Sync before rename, or the rename can land before the bytes do — which is
	// the same truncated file this function exists to prevent.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
