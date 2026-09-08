package security

// Security provides mTLS / dev-mode configuration.

type Config struct {
	DevMode    bool   `json:"dev_mode"`
	MTLSCert   string `json:"mtls_cert"`
	MTLSKey    string `json:"mtls_key"`
	CertDir    string `json:"cert_dir"`
}

func Defaults() Config {
	return Config{
		DevMode: true,
	}
}

func IsDevMode(cfg Config) bool {
	return cfg.DevMode
}