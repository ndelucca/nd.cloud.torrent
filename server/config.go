package server

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/ndelucca/nd.cloud.torrent/configfile"
	"github.com/ndelucca/nd.cloud.torrent/engine"
)

// reconfigure applies a config to the engine and persists it. The engine goes
// first: if it rejects the config, nothing is written and the old one stands.
//
// ErrRestartRequired is the exception, and it has to be. Most settings cannot be
// applied to a running torrent client, so the engine refuses them — but refusing
// to *save* them too would leave no way to change the listen port at all short
// of editing cloud-torrent.json by hand. That is not deferring the feature, it
// is removing it. So the config is persisted and the error is passed up, where
// classify turns it into a 200 telling the user to restart.
// Callers must hold configMu.
func (s *Server) reconfigure(c engine.Config) error {
	c, applyErr := s.applyConfig(c)
	if applyErr != nil && !errors.Is(applyErr, engine.ErrRestartRequired) {
		return applyErr
	}
	if err := configfile.Save(s.opts.ConfigPath, c); err != nil {
		return err
	}
	// Only after the write succeeds: desired must never claim something the file
	// does not hold, or a restart would silently revert what the form shows.
	s.desired = c
	return applyErr
}

// desiredConfig returns the configuration the user has asked for. The settings
// form renders this rather than the engine's live config, so a setting that
// needs a restart still shows the value that was saved.
func (s *Server) desiredConfig() engine.Config {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	return s.desired
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
