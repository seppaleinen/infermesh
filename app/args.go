package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// buildAutoRoutingArgs converts Settings.AutoRouting to CLI flags for the router.
// Returns nil when auto routing is disabled (AutoRouting is nil or Enabled is false).
// Tier values are trimmed so whitespace-only entries are skipped, mirroring the
// router's buildAutoRoutingConfig which also trims.
func buildAutoRoutingArgs(s Settings) []string {
	if s.AutoRouting == nil || !s.AutoRouting.Enabled {
		return nil
	}
	var args []string
	if v := strings.TrimSpace(s.AutoRouting.TierSimple); v != "" {
		args = append(args, "--auto-tier-simple", v)
	}
	if v := strings.TrimSpace(s.AutoRouting.TierMedium); v != "" {
		args = append(args, "--auto-tier-medium", v)
	}
	if v := strings.TrimSpace(s.AutoRouting.TierComplex); v != "" {
		args = append(args, "--auto-tier-complex", v)
	}
	if v := strings.TrimSpace(s.AutoRouting.TierHard); v != "" {
		args = append(args, "--auto-tier-hard", v)
	}
	if v := strings.TrimSpace(s.AutoRouting.TierHardest); v != "" {
		args = append(args, "--auto-tier-hardest", v)
	}
	if s.AutoRouting.UseFourTier {
		args = append(args, "--auto-use-four-tier")
	}
	if s.AutoRouting.SimpleMaxTokens > 0 {
		args = append(args, "--auto-simple-max-tokens", strconv.Itoa(s.AutoRouting.SimpleMaxTokens))
	}
	if s.AutoRouting.MediumMaxTokens > 0 {
		args = append(args, "--auto-medium-max-tokens", strconv.Itoa(s.AutoRouting.MediumMaxTokens))
	}
	return args
}

// buildRouterArgs constructs the CLI flags for the router binary from the
// saved Settings. No secrets are logged here.
func (s *Supervisor) buildRouterArgs(settings Settings) ([]string, error) {
	var args []string
	if modeFromSettings(settings).IsDev() {
		args = append(args, "--dev-mode")
	} else {
		args = append(args, "--prod-mode")
	}
	if settings.RouterAddr != "" {
		args = append(args, "--addr", settings.RouterAddr)
	}
	if settings.RelayURL != "" {
		args = append(args, "--relay-url", settings.RelayURL)
	}
	// Model aliases: convert the map to the CLI's comma-separated
	// "alias=canonical[@quant]" format. Empty map → flag omitted
	// (feature disabled, matching the nil-default contract).
	if len(settings.ModelAliases) > 0 {
		parts := make([]string, 0, len(settings.ModelAliases))
		for alias, canonical := range settings.ModelAliases {
			if alias == "" || canonical == "" {
				continue
			}
			parts = append(parts, alias+"="+canonical)
		}
		if len(parts) > 0 {
			args = append(args, "--model-alias", strings.Join(parts, ","))
		}
	}
	// Auto-routing: add flags if enabled.
	if autoArgs := buildAutoRoutingArgs(settings); len(autoArgs) > 0 {
		args = append(args, autoArgs...)
	}
	if !modeFromSettings(settings).IsDev() {
		apiKey, err := s.resolveSecret(secretRefRouterAPIKey)
		if err != nil {
			return nil, err
		}
		if apiKey != "" {
			args = append(args, "--api-key", apiKey)
		}
		cert, key, err := s.writeMTLSFiles()
		if err != nil {
			return nil, err
		}
		if cert != "" {
			args = append(args, "--mtls-cert", cert)
		}
		if key != "" {
			args = append(args, "--mtls-key", key)
		}
		// Write CA cert to a temp dir so the router can verify client certs.
		certDir, err := s.writeCertDir()
		if err != nil {
			return nil, err
		}
		if certDir != "" {
			args = append(args, "--cert-dir", certDir)
		}
		// Mint the desktop client certificate so RouterClient can connect
		// over mTLS in production mode.
		if certDir != "" {
			caCertPath := filepath.Join(certDir, "ca.crt")
			if _, _, err := s.writeClientCert(settings, caCertPath); err != nil {
				return nil, err
			}
		}
	}
	return args, nil
}

// buildWorkerArgs constructs the CLI flags for the worker binary.
func (s *Supervisor) buildWorkerArgs(settings Settings) ([]string, error) {
	var args []string
	if modeFromSettings(settings).IsDev() {
		args = append(args, "--dev-mode")
	} else {
		args = append(args, "--prod-mode")
	}
	if settings.WorkerBackend != "" {
		args = append(args, "--backend", settings.WorkerBackend)
	}
	if settings.WorkerModelPath != "" {
		args = append(args, "--model-path", settings.WorkerModelPath)
	}
	if settings.WorkerPort > 0 {
		args = append(args, "--port", strconv.Itoa(settings.WorkerPort))
	}
	if settings.WorkerMaxTokens > 0 {
		args = append(args, "--max-tokens", strconv.Itoa(settings.WorkerMaxTokens))
	}
	if modeFromSettings(settings).IsDev() {
		// dev-mode HTTP registration against the local router (skips mDNS).
		args = append(args, "--router", routerURLForWorker(settings.RouterAddr))
	} else {
		// prod-mode mTLS registration (bypasses mDNS).
		args = append(args, "--prod-router", routerURLForWorker(settings.RouterAddr))
	}
	if settings.RelayURL != "" {
		args = append(args, "--relay-url", settings.RelayURL)
	}
	if settings.WorkerEnableHealthChecks != nil {
		args = append(args, "--enable-health-checks", strconv.FormatBool(*settings.WorkerEnableHealthChecks))
	}
	if !modeFromSettings(settings).IsDev() {
		cert, key, err := s.writeMTLSFiles()
		if err != nil {
			return nil, err
		}
		if cert != "" {
			args = append(args, "--mtls-cert", cert)
		}
		if key != "" {
			args = append(args, "--mtls-key", key)
		}
	}
	return args, nil
}

// buildEnv returns the child process environment. We inherit the app's env
// and, when the worker has a configured custom-auth secret, surface it under
// a documented env var (the worker's `custom` backend can consume it). The
// value is resolved from the keyring and is never logged.
func (s *Supervisor) buildEnv(settings Settings) ([]string, error) {
	env := os.Environ()
	if auth, err := s.resolveSecret(secretRefWorkerCustomAuth); err != nil {
		return nil, err
	} else if auth != "" {
		env = append(env, envWorkerCustomAuth+"="+auth)
	}
	return env, nil
}
