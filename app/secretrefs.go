package main

// Secret refs and env-var names used when building child process args.
// The refs mirror knownSecretRefs in config_service.go so the YAML only ever
// holds references; values come from the keyring at process-launch time
// (issue #103 R2).
const (
	secretRefRouterAPIKey     = "router/apikey"
	secretRefWorkerCustomAuth = "worker/customauth"
	secretRefMTLSCert         = "mtls/cert"
	secretRefMTLSKey          = "mtls/key"
	secretRefMTLSCACert       = "mtls/ca-cert"
	secretRefMTLSCAKey        = "mtls/ca-key"
	secretRefRelayCreds       = "relay/creds"
	envWorkerCustomAuth       = "INFERMESH_WORKER_CUSTOM_AUTH"
)

// SecretRefs is the single source of truth for keyring secret-reference keys.
// Both ConfigService (validation) and Supervisor (resolution) read from this
// slice, so the YAML can only ever hold references that the app actually
// consumes (issue #103 R2).
var SecretRefs = []string{
	secretRefRouterAPIKey,
	secretRefWorkerCustomAuth,
	secretRefMTLSCert,
	secretRefMTLSKey,
	secretRefMTLSCACert,
	secretRefMTLSCAKey,
	secretRefRelayCreds,
}

// knownSecretRefs reports whether ref is a recognised keyring secret reference.
// Replaces the per-file map in config_service.go (issue #103 R2).
func knownSecretRefs(ref string) bool {
	for _, s := range SecretRefs {
		if s == ref {
			return true
		}
	}
	return false
}