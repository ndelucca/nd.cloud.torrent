package engine

import "fmt"

// Config is the user-editable engine configuration. It is persisted by the
// server as JSON and round-trips through the settings form in the web UI, so
// exported field names are part of the wire contract.
type Config struct {
	AutoStart         bool
	DisableEncryption bool
	DownloadDirectory string
	EnableUpload      bool
	EnableSeeding     bool
	IncomingPort      int
}

// Validate reports whether the config can be applied. It must be called before
// any existing client is torn down — see Engine.Configure.
func (c Config) Validate() error {
	if c.DownloadDirectory == "" {
		return fmt.Errorf("Download directory is required")
	}
	if c.IncomingPort <= 0 || c.IncomingPort > 65535 {
		return fmt.Errorf("Invalid incoming port (%d)", c.IncomingPort)
	}
	return nil
}
