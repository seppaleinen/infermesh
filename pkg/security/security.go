package security

// Security provides mTLS / dev-mode configuration.
type Config struct {
	DevMode            bool   `json:"dev_mode"`
	MTLSCert           string `json:"mtls_cert"`
	MTLSKey            string `json:"mtls_key"`
	CertDir            string `json:"cert_dir"`
	APIKey             string `json:"api_key"`
	TrustedCNs         string `json:"trusted_cns"` // comma-separated CNs allowed to present client certs (empty = any)
	EnableHealthChecks bool   `json:"enable_health_checks"` // Controls periodic backend health check loop
}

func Defaults() Config {
	return Config{
		DevMode:            true,
		EnableHealthChecks: true,
	}
}

func IsDevMode(cfg Config) bool {
	return cfg.DevMode
}