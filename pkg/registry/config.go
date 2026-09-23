package registry

import (
	"fmt"
	"time"
)

// Config controls registry behavior.
type Config struct {
	UnavailableTTL time.Duration // mark unavailable after this much silence
	RemoveTTL      time.Duration // remove from map after this much silence
	CheckInterval  time.Duration // how often to sweep for stale workers
}

// Defaults returns sensible defaults.
func Defaults() Config {
	return Config{
		UnavailableTTL: 60 * time.Second,
		RemoveTTL:      120 * time.Second,
		CheckInterval:  5 * time.Second,
	}
}

// Validate checks the config for sensible values.
func (c Config) Validate() error {
	if c.UnavailableTTL < 1*time.Second {
		return fmt.Errorf("unavailable_ttl must be >= 1s")
	}
	if c.RemoveTTL < c.UnavailableTTL {
		return fmt.Errorf("remove_ttl must be >= unavailable_ttl")
	}
	if c.CheckInterval < 500*time.Millisecond {
		return fmt.Errorf("check_interval must be >= 500ms")
	}
	return nil
}
