package server

import (
	"fmt"
	"path/filepath"

	"github.com/ndelucca/nd.cloud.torrent/configfile"
	"github.com/ndelucca/nd.cloud.torrent/engine"
)

// reconfigure applies a config to the engine and persists it. The engine restart
// happens first: if it fails, nothing is written and the old config stands.
func (s *Server) reconfigure(c engine.Config) error {
	c, err := s.applyConfig(c)
	if err != nil {
		return err
	}
	return configfile.Save(s.opts.ConfigPath, c)
}

// applyConfig absolutizes the download directory and hands the config to the
// engine, returning what was actually applied. It writes nothing: startup uses
// it alone, so a run that changes no settings leaves the config file untouched.
func (s *Server) applyConfig(c engine.Config) (engine.Config, error) {
	dldir, err := filepath.Abs(c.DownloadDirectory)
	if err != nil {
		return c, fmt.Errorf("invalid path: %w", err)
	}
	c.DownloadDirectory = dldir
	if err := s.engine.Configure(c); err != nil {
		return c, err
	}
	return c, nil
}
