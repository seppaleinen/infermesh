package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// configArgPair finds flag in args and returns the element following it (or "").
func configArgPair(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func configArgsContain(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// TestConfigSaveLoadRoundTrip verifies the SaveTo -> LoadFrom round trip:
// every non-secret field survives, secret values never reach the YAML, and
// SecretRefs survive.
func TestConfigSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yml")

	s := DefaultSettings()
	s.RouterAddr = ":9010"
	s.WorkerBackend = "vllm"
	s.WorkerModelPath = "/models/round-trip.bin"
	s.WorkerPort = 9011
	s.WorkerEnableHealthChecks = boolPtr(false)
	s.RelayURL = "ws://relay.example:8080"
	s.ModelAliases = map[string]string{"short-alias": "canonical-model"}
	prod := false
	s.DevMode = &prod
	s.ClientCertCN = "desktop-client"
	s.AutoStartOnLogin = boolPtr(true)
	s.AutoRouting = &AutoRoutingSettings{
		Enabled:         true,
		UseFourTier:     true,
		TierSimple:      "small-model",
		TierMedium:      "medium-model",
		TierComplex:     "large-model",
		TierHard:        "hard-model",
		TierHardest:     "hardest-model",
		SimpleMaxTokens: 150,
		MediumMaxTokens: 500,
	}
	s.Secrets = map[string]string{"router/apikey": "must-not-leak"}
	s.SecretRefs = map[string]string{"router/apikey": "router/apikey"}

	if err := SaveTo(s, path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings file: %v", err)
	}
	if strings.Contains(string(raw), "must-not-leak") {
		t.Fatal("secret value leaked into YAML")
	}

	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	loaded = mergeDefaults(loaded)

	if loaded.RouterAddr != s.RouterAddr {
		t.Errorf("RouterAddr: got %q want %q", loaded.RouterAddr, s.RouterAddr)
	}
	if loaded.WorkerBackend != s.WorkerBackend {
		t.Errorf("WorkerBackend: got %q want %q", loaded.WorkerBackend, s.WorkerBackend)
	}
	if loaded.WorkerModelPath != s.WorkerModelPath {
		t.Errorf("WorkerModelPath: got %q want %q", loaded.WorkerModelPath, s.WorkerModelPath)
	}
	if loaded.WorkerPort != s.WorkerPort {
		t.Errorf("WorkerPort: got %d want %d", loaded.WorkerPort, s.WorkerPort)
	}
	if loaded.WorkerEnableHealthChecks == nil || *loaded.WorkerEnableHealthChecks {
		t.Errorf("WorkerEnableHealthChecks: got %v want false", loaded.WorkerEnableHealthChecks)
	}
	if loaded.RelayURL != s.RelayURL {
		t.Errorf("RelayURL: got %q want %q", loaded.RelayURL, s.RelayURL)
	}
	if !reflect.DeepEqual(loaded.ModelAliases, s.ModelAliases) {
		t.Errorf("ModelAliases: got %v want %v", loaded.ModelAliases, s.ModelAliases)
	}
	if loaded.DevMode == nil || *loaded.DevMode {
		t.Errorf("DevMode: got %v want explicit false", loaded.DevMode)
	}
	if loaded.ClientCertCN != s.ClientCertCN {
		t.Errorf("ClientCertCN: got %q want %q", loaded.ClientCertCN, s.ClientCertCN)
	}
	if loaded.AutoStartOnLogin == nil || !*loaded.AutoStartOnLogin {
		t.Errorf("AutoStartOnLogin: got %v want explicit true", loaded.AutoStartOnLogin)
	}
	if loaded.AutoRouting == nil {
		t.Fatal("AutoRouting: got nil, want non-nil")
	}
	if !loaded.AutoRouting.Enabled {
		t.Errorf("AutoRouting.Enabled = %v, want true", loaded.AutoRouting.Enabled)
	}
	if !loaded.AutoRouting.UseFourTier {
		t.Errorf("AutoRouting.UseFourTier = %v, want true", loaded.AutoRouting.UseFourTier)
	}
	if loaded.AutoRouting.TierSimple != s.AutoRouting.TierSimple {
		t.Errorf("AutoRouting.TierSimple = %q, want %q", loaded.AutoRouting.TierSimple, s.AutoRouting.TierSimple)
	}
	if loaded.AutoRouting.TierMedium != s.AutoRouting.TierMedium {
		t.Errorf("AutoRouting.TierMedium = %q, want %q", loaded.AutoRouting.TierMedium, s.AutoRouting.TierMedium)
	}
	if loaded.AutoRouting.TierComplex != s.AutoRouting.TierComplex {
		t.Errorf("AutoRouting.TierComplex = %q, want %q", loaded.AutoRouting.TierComplex, s.AutoRouting.TierComplex)
	}
	if loaded.AutoRouting.TierHard != s.AutoRouting.TierHard {
		t.Errorf("AutoRouting.TierHard = %q, want %q", loaded.AutoRouting.TierHard, s.AutoRouting.TierHard)
	}
	if loaded.AutoRouting.TierHardest != s.AutoRouting.TierHardest {
		t.Errorf("AutoRouting.TierHardest = %q, want %q", loaded.AutoRouting.TierHardest, s.AutoRouting.TierHardest)
	}
	if loaded.AutoRouting.SimpleMaxTokens != s.AutoRouting.SimpleMaxTokens {
		t.Errorf("AutoRouting.SimpleMaxTokens = %d, want %d", loaded.AutoRouting.SimpleMaxTokens, s.AutoRouting.SimpleMaxTokens)
	}
	if loaded.AutoRouting.MediumMaxTokens != s.AutoRouting.MediumMaxTokens {
		t.Errorf("AutoRouting.MediumMaxTokens = %d, want %d", loaded.AutoRouting.MediumMaxTokens, s.AutoRouting.MediumMaxTokens)
	}
	if len(loaded.Secrets) != 0 {
		t.Errorf("Secrets should be empty after load (yaml:\"-\"), got %v", loaded.Secrets)
	}
	if loaded.SecretRefs["router/apikey"] != "router/apikey" {
		t.Errorf("SecretRefs[router/apikey] missing or wrong: %v", loaded.SecretRefs)
	}
}

// TestConfigMergeDefaultsFillsMissing verifies mergeDefaults fills zero-value
// fields with the documented defaults while leaving explicitly-set fields
// (including explicit false booleans) untouched.
func TestConfigMergeDefaultsFillsMissing(t *testing.T) {
	t.Run("zero settings get defaults", func(t *testing.T) {
		merged := mergeDefaults(Settings{})
		def := DefaultSettings()

		if merged.RouterAddr != def.RouterAddr {
			t.Errorf("RouterAddr: got %q want %q", merged.RouterAddr, def.RouterAddr)
		}
		if merged.WorkerBackend != def.WorkerBackend {
			t.Errorf("WorkerBackend: got %q want %q", merged.WorkerBackend, def.WorkerBackend)
		}
		if merged.WorkerPort != def.WorkerPort {
			t.Errorf("WorkerPort: got %d want %d", merged.WorkerPort, def.WorkerPort)
		}
		if merged.WorkerEnableHealthChecks == nil || !*merged.WorkerEnableHealthChecks {
			t.Errorf("WorkerEnableHealthChecks: got %v want true", merged.WorkerEnableHealthChecks)
		}
		if merged.DevMode == nil || !*merged.DevMode {
			t.Errorf("DevMode: got %v want true", merged.DevMode)
		}
		if merged.ClientCertCN != def.ClientCertCN {
			t.Errorf("ClientCertCN: got %q want %q", merged.ClientCertCN, def.ClientCertCN)
		}
		if merged.AutoStartOnLogin == nil || *merged.AutoStartOnLogin {
			t.Errorf("AutoStartOnLogin: got %v want false (opt-in)", merged.AutoStartOnLogin)
		}
		if merged.Secrets == nil {
			t.Error("Secrets nil after merge (would panic on write)")
		}
		if merged.SecretRefs == nil {
			t.Error("SecretRefs nil after merge (would panic on write)")
		}
		if merged.ModelAliases == nil {
			t.Error("ModelAliases nil after merge")
		}
	})

	t.Run("explicit non-default values are preserved", func(t *testing.T) {
		prod := false
		autostart := true
		merged := mergeDefaults(Settings{
			RouterAddr:               ":7777",
			WorkerBackend:            "custom",
			WorkerPort:               7778,
			WorkerEnableHealthChecks: boolPtr(false),
			DevMode:                  &prod,
			ClientCertCN:             "custom-cn",
			AutoStartOnLogin:         &autostart,
		})
		if merged.RouterAddr != ":7777" {
			t.Errorf("RouterAddr overwritten: got %q", merged.RouterAddr)
		}
		if merged.WorkerBackend != "custom" {
			t.Errorf("WorkerBackend overwritten: got %q", merged.WorkerBackend)
		}
		if merged.WorkerPort != 7778 {
			t.Errorf("WorkerPort overwritten: got %d", merged.WorkerPort)
		}
		if merged.WorkerEnableHealthChecks == nil || *merged.WorkerEnableHealthChecks {
			t.Errorf("explicit false WorkerEnableHealthChecks clobbered: %v", merged.WorkerEnableHealthChecks)
		}
		if merged.DevMode == nil || *merged.DevMode {
			t.Errorf("explicit false DevMode clobbered: %v", merged.DevMode)
		}
		if merged.ClientCertCN != "custom-cn" {
			t.Errorf("ClientCertCN overwritten: got %q", merged.ClientCertCN)
		}
		if merged.AutoStartOnLogin == nil || !*merged.AutoStartOnLogin {
			t.Errorf("explicit true AutoStartOnLogin clobbered: %v", merged.AutoStartOnLogin)
		}
	})

	t.Run("nil maps are initialised", func(t *testing.T) {
		merged := mergeDefaults(Settings{})
		if merged.Secrets == nil || merged.SecretRefs == nil || merged.ModelAliases == nil {
			t.Error("nil maps not initialised by mergeDefaults")
		}
		// Writing back through SaveTo must not panic on these maps.
		if err := SaveTo(merged, filepath.Join(t.TempDir(), "settings.yml")); err != nil {
			t.Errorf("SaveTo of merged zero settings: %v", err)
		}
	})
}

// TestConfigModelAliasesBuildRouterArg verifies that saved ModelAliases produce
// the --model-alias CLI flag on the router in "alias=canonical,alias=canonical"
// form, and that an empty alias map omits the flag entirely.
func TestConfigModelAliasesBuildRouterArg(t *testing.T) {
	fx := newTestSupervisor(t)
	fx.settings.ModelAliases = map[string]string{
		"gemma": "canonical-gemma",
		"llama": "canonical-llama",
	}
	fx.save(t)

	if err := fx.s.StartRouter(); err != nil {
		t.Fatalf("StartRouter: %v", err)
	}
	args := fx.builder.last().Args()

	if !configArgsContain(args, "--model-alias") {
		t.Fatalf("router args missing --model-alias: %q", args)
	}
	got := configArgPair(args, "--model-alias")
	parts := strings.Split(got, ",")
	if len(parts) != 2 {
		t.Fatalf("--model-alias = %q, want 2 comma-separated pairs", got)
	}
	seen := map[string]bool{}
	for _, p := range parts {
		seen[p] = true
	}
	if !seen["gemma=canonical-gemma"] {
		t.Errorf("--model-alias missing \"gemma=canonical-gemma\": %q", got)
	}
	if !seen["llama=canonical-llama"] {
		t.Errorf("--model-alias missing \"llama=canonical-llama\": %q", got)
	}
}

func TestConfigNoModelAliasesOmitsFlag(t *testing.T) {
	fx := newTestSupervisor(t)
	fx.save(t) // DefaultSettings: empty ModelAliases map

	if err := fx.s.StartRouter(); err != nil {
		t.Fatalf("StartRouter: %v", err)
	}
	args := fx.builder.last().Args()
	if configArgsContain(args, "--model-alias") {
		t.Errorf("router args contain --model-alias with empty alias map: %q", args)
	}
}

// TestConfigAPIKeyBuildRouterArg verifies that in prod mode the router is
// launched with --api-key resolved from the keyring (router/apikey secret), and
// that dev mode never carries an --api-key flag.
func TestConfigAPIKeyBuildRouterArg(t *testing.T) {
	const apiKey = "roundtrip-api-key"

	t.Run("prod mode resolves api key from keyring", func(t *testing.T) {
		fx := newTestSupervisor(t)
		fx.kr.Set(secretRefRouterAPIKey, apiKey)
		prod := false
		fx.settings.DevMode = &prod
		fx.save(t)

		if err := fx.s.StartRouter(); err != nil {
			t.Fatalf("StartRouter: %v", err)
		}
		args := fx.builder.last().Args()

		if !configArgsContain(args, "--prod-mode") {
			t.Errorf("prod-mode args missing --prod-mode: %q", args)
		}
		if got := configArgPair(args, "--api-key"); got != apiKey {
			t.Errorf("--api-key = %q, want %q (args %q)", got, apiKey, args)
		}
	})

	t.Run("dev mode omits api key", func(t *testing.T) {
		fx := newTestSupervisor(t)
		fx.kr.Set(secretRefRouterAPIKey, apiKey)
		fx.save(t) // DefaultSettings: dev mode

		if err := fx.s.StartRouter(); err != nil {
			t.Fatalf("StartRouter: %v", err)
		}
		args := fx.builder.last().Args()
		if configArgsContain(args, "--dev-mode") != true {
			t.Errorf("dev-mode args missing --dev-mode: %q", args)
		}
		if configArgsContain(args, "--api-key") {
			t.Errorf("dev-mode args must not contain --api-key: %q", args)
		}
	})
}

// TestConfigMTLSBuildArgs verifies that in prod mode both router and worker
// receive --mtls-cert/--mtls-key pointing at temp files holding the keyring
// secret values, and that dev mode omits the mTLS flags.
func TestConfigMTLSBuildArgs(t *testing.T) {
	const certPEM = "-----BEGIN CERTIFICATE-----\nMTLS-TEST-CERT\n-----END CERTIFICATE-----\n"
	const keyPEM = "-----BEGIN PRIVATE KEY-----\nMTLS-TEST-KEY\n-----END PRIVATE KEY-----\n"
	prod := false

	t.Run("router prod mode gets mtls flags", func(t *testing.T) {
		fx := newTestSupervisor(t)
		fx.kr.Set(secretRefMTLSCert, certPEM)
		fx.kr.Set(secretRefMTLSKey, keyPEM)
		fx.settings.DevMode = &prod
		fx.save(t)

		if err := fx.s.StartRouter(); err != nil {
			t.Fatalf("StartRouter: %v", err)
		}
		args := fx.builder.last().Args()

		certPath := configArgPair(args, "--mtls-cert")
		keyPath := configArgPair(args, "--mtls-key")
		if certPath == "" || keyPath == "" {
			t.Fatalf("router args missing --mtls-cert/--mtls-key: %q", args)
		}
		assertFileModeAndContent(t, certPath, certPEM)
		assertFileModeAndContent(t, keyPath, keyPEM)
	})

	t.Run("worker prod mode gets mtls flags", func(t *testing.T) {
		fx := newTestSupervisor(t)
		fx.kr.Set(secretRefMTLSCert, certPEM)
		fx.kr.Set(secretRefMTLSKey, keyPEM)
		fx.settings.DevMode = &prod
		fx.save(t)

		if err := fx.s.StartWorker(); err != nil {
			t.Fatalf("StartWorker: %v", err)
		}
		args := fx.builder.last().Args()

		certPath := configArgPair(args, "--mtls-cert")
		keyPath := configArgPair(args, "--mtls-key")
		if certPath == "" || keyPath == "" {
			t.Fatalf("worker args missing --mtls-cert/--mtls-key: %q", args)
		}
		assertFileModeAndContent(t, certPath, certPEM)
		assertFileModeAndContent(t, keyPath, keyPEM)
	})

	t.Run("dev mode omits mtls flags", func(t *testing.T) {
		fx := newTestSupervisor(t)
		fx.kr.Set(secretRefMTLSCert, certPEM)
		fx.kr.Set(secretRefMTLSKey, keyPEM)
		fx.save(t) // DefaultSettings: dev mode

		if err := fx.s.StartRouter(); err != nil {
			t.Fatalf("StartRouter: %v", err)
		}
		if err := fx.s.StartWorker(); err != nil {
			t.Fatalf("StartWorker: %v", err)
		}
		if args := fx.builder.at(0).Args(); configArgsContain(args, "--mtls-cert") || configArgsContain(args, "--mtls-key") {
			t.Errorf("router dev-mode args contain mTLS flags: %q", args)
		}
		if args := fx.builder.at(1).Args(); configArgsContain(args, "--mtls-cert") || configArgsContain(args, "--mtls-key") {
			t.Errorf("worker dev-mode args contain mTLS flags: %q", args)
		}
	})

	t.Run("prod mode without mtls secrets omits flags", func(t *testing.T) {
		fx := newTestSupervisor(t)
		fx.settings.DevMode = &prod
		fx.save(t) // no mtls secrets set

		if err := fx.s.StartRouter(); err != nil {
			t.Fatalf("StartRouter: %v", err)
		}
		args := fx.builder.last().Args()
		if configArgsContain(args, "--mtls-cert") {
			t.Errorf("router args contain --mtls-cert without mtls secret: %q", args)
		}
	})
}

// TestConfigInvalidSettingsTolerantLoad verifies tolerant loading: a missing
// settings file is not an error (returns initialised zero Settings so the
// caller can write defaults), and a partially-populated file merges cleanly.
func TestConfigInvalidSettingsTolerantLoad(t *testing.T) {
	t.Run("missing file is not an error", func(t *testing.T) {
		s, err := LoadFrom(filepath.Join(t.TempDir(), "does-not-exist.yml"))
		if err != nil {
			t.Fatalf("LoadFrom on missing file: %v, want nil", err)
		}
		// Zero-value fields; maps initialised so SaveTo cannot panic.
		if s.RouterAddr != "" || s.WorkerPort != 0 {
			t.Errorf("expected zero Settings for missing file, got %+v", s)
		}
		merged := mergeDefaults(s)
		if err := SaveTo(merged, filepath.Join(t.TempDir(), "settings.yml")); err != nil {
			t.Errorf("SaveTo of merged zero settings (missing-file path): %v", err)
		}
	})

	t.Run("partial file merges with defaults", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.yml")
		// Only a couple of fields set on disk.
		partial := DefaultSettings()
		partial.RouterAddr = ":9030"
		partial.WorkerPort = 9031
		if err := SaveTo(partial, path); err != nil {
			t.Fatalf("SaveTo partial: %v", err)
		}

		loaded, err := LoadFrom(path)
		if err != nil {
			t.Fatalf("LoadFrom partial: %v", err)
		}
		merged := mergeDefaults(loaded)
		if merged.RouterAddr != ":9030" {
			t.Errorf("RouterAddr: got %q want :9030", merged.RouterAddr)
		}
		if merged.WorkerPort != 9031 {
			t.Errorf("WorkerPort: got %d want 9031", merged.WorkerPort)
		}
		// Untouched fields come from defaults.
		def := DefaultSettings()
		if merged.WorkerBackend != def.WorkerBackend {
			t.Errorf("WorkerBackend: got %q want default %q", merged.WorkerBackend, def.WorkerBackend)
		}
	})

	t.Run("load never crashes and stays saveable", func(t *testing.T) {
		s, err := LoadFrom(filepath.Join(t.TempDir(), "nope.yml"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		merged := mergeDefaults(s)
		// Writing the tolerant-load result must succeed (nil-map guard).
		if err := SaveTo(merged, filepath.Join(t.TempDir(), "out.yml")); err != nil {
			t.Errorf("SaveTo after tolerant load: %v", err)
		}
	})
}

// TestConfigWorkerCustomAuthBuildEnv verifies the worker's custom-auth secret
// surfaces as the INFERMESH_WORKER_CUSTOM_AUTH env var (not a CLI flag) on the
// child environment.
func TestConfigWorkerCustomAuthBuildEnv(t *testing.T) {
	fx := newTestSupervisor(t)
	fx.kr.Set(secretRefWorkerCustomAuth, "worker-auth-token")
	fx.save(t)

	if err := fx.s.StartWorker(); err != nil {
		t.Fatalf("StartWorker: %v", err)
	}
	env := fx.builder.last().Env()
	found := false
	for _, kv := range env {
		if kv == envWorkerCustomAuth+"=worker-auth-token" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("worker env missing %s=worker-auth-token", envWorkerCustomAuth)
	}
}
