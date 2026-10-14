# App-to-Router TLS Trust Boundary (v1)

## Overview

This contract defines how the InferMesh desktop app (Wails) communicates with the
router binary over mTLS in production mode. The desktop app is not a worker;
it is a management client that queries the router's management API
(`/v1/workers`, `/meta/models/popular`, `/meta/connections/count`).

The router is the trust anchor. The app must present a client certificate
signed by the same CA that signs the router's server certificate. The router
verifies the client certificate using the CA certificate in `--cert-dir/ca.crt`
and, if `--trusted-cn` is configured, checks the client certificate's CN.

## Certificate lifecycle

### 1. CA provisioning
The user provisions the CA via the OS keyring. Two secrets are required for
production mode:

| Ref | Purpose |
|-----|---------|
| `mtls/ca-cert` | PEM-encoded CA certificate (`-----BEGIN CERTIFICATE-----`). Written to the router's `--cert-dir/ca.crt` at startup. The router uses it to verify client certificates. |
| `mtls/ca-key` | PEM-encoded CA private key. Used by the supervisor to mint the desktop client certificate. Never leaves the keyring; the in-memory PEM is written to a temp file, used to sign the client certificate, and deleted on shutdown. |

The CA certificate and key are generated once with `security.GenerateSelfSignedCA`
or imported by the user. The CA is valid for 10 years.

### 2. Router certificate
The router's own server certificate is provisioned via the keyring as:

| Ref | Purpose |
|-----|---------|
| `mtls/cert` | PEM-encoded server certificate (leaf, signed by the CA). Passed to the router as `--mtls-cert`. |
| `mtls/key` | PEM-encoded server private key. Passed to the router as `--mtls-key`. |

The router's certificate SANs must include the address the desktop app
connects to (e.g. `127.0.0.1` and `localhost`). `security.GenerateNodeCert`
produces SANs that include both `localhost` (DNS) and `127.0.0.1` (IP), so
the desktop client verifies the server cert against the CA pool **without**
`InsecureSkipVerify` (see below).

### 3. Desktop client certificate
The supervisor mints a desktop client certificate on every router start in
production mode.

* CN: `Settings.ClientCertCN`, default `infermesh-desktop`.
* SANs: `localhost` and the machine's primary IPv4 (produced by
  `security.GenerateNodeCert`).
* Key usage: `DigitalSignature | KeyEncipherment`, ExtKeyUsage
  `ClientAuth | ServerAuth`.
* Signed by: the CA key from `mtls/ca-key`.
* Lifetime: 10 years (same as the CA). The certificate is regenerated on every
  router start from the CA key, so it is effectively ephemeral — the PEM files
  are written to a temp directory and deleted on shutdown.

The supervisor stores the client certificate PEM paths in `Supervisor.clientCertPaths`
and the CA cert directory path in `Supervisor.caCertDir`. The `RouterClient` uses
these paths to build an `http.Client` with a TLS transport that presents the client
certificate and trusts the CA certificate.

## Router behaviour

When the router is started in production mode with `--cert-dir`, `--mtls-cert`,
and `--mtls-key`:

1. The router validates the TLS configuration (`security.ValidateTLSConfig`).
2. The router loads the CA certificate from `--cert-dir/ca.crt` and builds an
   `x509.CertPool`.
3. The router wraps the HTTP mux with `security.NewMTLSMiddleware(caPool, true,
   ...)`. The middleware requires a valid client certificate signed by the CA.
4. The router starts an HTTPS server with `tls.Config{ClientAuth:
   RequireAndVerifyClientCert, ClientCAs: caPool}`.
5. The router accepts `--trusted-cn` (comma-separated). If provided, the
   middleware restricts accepted client certificates to those CNs. The
   desktop app's CN (`infermesh-desktop`) must be in the allowed set.

   **`--trusted-cn` is opt-in and NOT passed by the desktop supervisor.**
   `app/args.go::buildRouterArgs()` emits only `--prod-mode`, `--addr`,
   `--mtls-cert`, `--mtls-key`, `--cert-dir`, and `--api-key` — it never
   appends `--trusted-cn`. The router therefore accepts any certificate
   signed by the CA, including worker registration certs (which carry their
   own CN, e.g. `worker-1`). Tightening trust via `--trusted-cn` is a manual
   router-launch option only; the desktop cannot use it without also
   enumerating every worker CN, which it does not know.

The router's management endpoints (`/v1/workers`, `/meta/*`) are protected
by mTLS. The router's inference endpoints (`/v1/chat/completions`, etc.)
are also protected by mTLS and, if configured, by the API key middleware
that wraps the mTLS middleware.

## Desktop app behaviour

### RouterClient
`RouterClient` is a Wails service bound to the frontend. In production mode:

* `effectiveBaseURL()` upgrades `http://` to `https://` when the supervisor has
  minted a client certificate.
* `httpClient()` builds a `*http.Client` with:
  - `Certificates`: the desktop client certificate loaded from `Supervisor.clientCertPaths`.
  - `RootCAs`: the CA certificate loaded from `Supervisor.caCertDir/ca.crt`.
  - No `InsecureSkipVerify` — the server cert SANs include `127.0.0.1` (IP),
    so hostname verification succeeds when dialing `https://127.0.0.1:...`.
* `workersPath()`, `popularModelsPath()`, `connectionCountPath()` all use
  `effectiveBaseURL()`.
* All HTTP calls (`GetWorkers`, `GetPopularModels`, `GetConnectionCount`)
  use `c.httpClient()` instead of `http.DefaultClient`.

### Supervisor
The supervisor is responsible for cert provisioning:

* `writeCertDir()` resolves `mtls/ca-cert` from the keyring, creates a temp
  directory, and writes it as `ca.crt`. The directory path is stored in
  `Supervisor.caCertDir` and passed to the router as `--cert-dir`.
* `writeClientCert(settings, caCertPath)` resolves `mtls/ca-key` from the
  keyring, writes it to a temp PEM, calls `security.GenerateNodeCert` with the
  CA cert/key and `settings.ClientCertCN`, and stores the resulting cert/key
  paths in `Supervisor.clientCertPaths`.
* `buildRouterArgs()` is called when the router is started. In production mode
  it writes the CA cert dir, mints the client certificate, and appends
  `--cert-dir <dir>` to the router args. The client certificate paths are
  stored on the supervisor for use by `RouterClient`.
* `StopRouter()` clears `clientCertPaths` and `caCertDir` so a subsequent
  dev-mode start uses plain HTTP.
* `cleanupTempFiles()` removes all temp directories and files created for the
  CA cert and client certificate.

The supervisor's `defaultRouterProbe` creates a `RouterClient` (fallback
for tests without `main.go`), calls `SetTransport(s.buildTLSClient())` when
the mTLS transport is available, and uses it for router health checks. This
ensures the supervisor's status indicators use the correct TLS transport.
(Issue #103 R1 removed the Supervisor reference from RouterClient.)

## Hostname verification note

The desktop client verifies the router's server certificate against the CA
pool **without** `InsecureSkipVerify`. The server certificate generated by
`security.GenerateNodeCert` (or user-provisioned) MUST include `127.0.0.1`
as an IP SAN (or `localhost` as a DNS SAN) to match the desktop's dial
address (`127.0.0.1:port` for local prod mode). This is the current
behaviour — no `InsecureSkipVerify` is set.

The original design considered `InsecureSkipVerify: true` as a fallback for
user-provisioned certs without matching SANs, but the default cert generator
(`GenerateNodeCert`) includes both `localhost` and `127.0.0.1`, making the
fallback unnecessary. If a user imports their own cert without the required
SANs, the connection will fail with a certificate verification error — this
is the correct behaviour and encourages proper certificate provisioning.

## Security considerations

* The CA private key never leaves the keyring. The in-memory PEM is written
  to a temp file with mode 0600, used to sign the client certificate, and
  deleted on shutdown.
* The client certificate is regenerated on every router start. No long-lived
  client key material is persisted to disk.
* The router's `--trusted-cn` flag can be tightened to only allow the
  desktop CN and known worker CNs. The default is to accept any certificate
  signed by the CA. The desktop supervisor does NOT pass `--trusted-cn`
  (see Router behaviour, step 5) — it relies on CA-chain verification alone.
* The desktop sends `X-API-Key` on **every** outbound request when a key is
  configured (issue #119). mTLS alone is NOT sufficient: `/meta/aliases` is
  in the API-key enforcement list (`wrapInferenceEndpoints`) and returns 401
  without a valid key. The other three management endpoints
  (`/v1/workers`, `/meta/models/popular`, `/meta/connections/count`) are
  NOT in the enforcement list and return 200 without a key — but the
  desktop sends the header on all four regardless, defensively.

## Acceptance criteria mapping

| Criteria | Implementation |
|----------|----------------|
| SettingsForm exposes dev/prod toggle; prod mode persists to settings | `Settings.DevMode` exists, merged with defaults, persisted to YAML; UI already shows mTLS fields when `devMode === false`. |
| RouterClient builds TLS transport with client cert and trusts ca.crt | `RouterClient.httpClient()` loads client cert/key from supervisor and CA pool from `caCertDir`. |
| Client cert provisioning: mint via CA key | `Supervisor.writeClientCert()` uses `security.GenerateNodeCert` with `mtls/ca-key`. |
| Supervisor starts router + worker in prod mode when opted in | `buildRouterArgs()` calls `writeCertDir()` and `writeClientCert()` in production mode; `buildWorkerArgs()` already handles `--prod-mode`. |
| Dev-mode HTTP path remains default and unchanged | `RouterClient.httpClient()` falls back to `http.DefaultClient` when no client cert paths are present. |
| Contract doc exists | This document. |
