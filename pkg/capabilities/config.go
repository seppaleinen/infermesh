package capabilities

import (
	"time"
)

// Config holds configuration for the capabilities package.
type Config struct {
	ModelConfigPath string
	SysfsPath       string
	ProcPath        string
	RefreshInterval time.Duration
}

// Defaults returns a Config with default values.
func Defaults() Config {
	return Config{
		SysfsPath:       "/sys",
		ProcPath:        "/proc",
		ModelConfigPath: "/etc/infermesh/models.yaml",
		RefreshInterval: 5 * time.Minute,
	}
}

// Validate checks the configuration for validity.
func (c *Config) Validate() error {
	if c.SysfsPath == "" {
		c.SysfsPath = "/sys"
	}
	if c.ProcPath == "" {
		c.ProcPath = "/proc"
	}
	if c.RefreshInterval <= 0 {
		c.RefreshInterval = 5 * time.Minute
	}
	return nil
}