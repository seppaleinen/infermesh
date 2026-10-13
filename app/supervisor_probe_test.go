package main

// Unit test for Supervisor.defaultRouterProbe (issue #104, Phase 0).
// The probe is the supervisor's live router-reachability check: it builds a
// fresh RouterClient per call (URL normalised from Settings.RouterAddr),
// attaches the supervisor for TLS negotiation, and reads /v1/workers. HTTP is
// mocked with httptest from the standard library.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/router"
	"github.com/seppaleinen/infermesh/pkg/security"
)

// newProbeSupervisor builds a Supervisor whose routerProbe is the REAL
// defaultRouterProbe (NewSupervisorService wires it), with the HTTP probes
// otherwise kept away from real routers by pointing RouterAddr at the test
// server.
func newProbeSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	s := NewSupervisorService(nil, t.TempDir()+"/settings.yml", t.TempDir(), t.TempDir())
	// NewSupervisorService already set s.routerProbe = s.defaultRouterProbe();
	// keep the real one under test. If it were nil, NewSupervisorService's
	// wiring regressed.
	if s.routerProbe == nil {
		t.Fatal("Supervisor.routerProbe is nil after NewSupervisorService")
	}
	return s
}

// TestDefaultRouterProbeBuildsClientAndReadsWorkers verifies the probe
// contract end-to-end against a mocked router: the URL is normalised from
// Settings.RouterAddr, the request lands on /v1/workers over plain GET, and
// the WorkerViews are returned. The probe is a pure read — it does NOT call
// Supervisor.StartWorker (issue #103 R1 fix).
func TestDefaultRouterProbeBuildsClientAndReadsWorkers(t *testing.T) {
	workersBody := router.WorkersResponse{Workers: []router.WorkerInfo{
		{
			ID:           "probe-w-1",
			Hostname:     "probe-host",
			IP:           "127.0.0.1",
			Port:         8081,
			Status:       "available",
			LoadedModels: []string{"probe-model"},
			LastSeen:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}}

	var gotPath, gotMethod string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(workersBody); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer ts.Close()

	// Point the probe at the test server via the normalised RouterAddr.
	// routerBaseURL("127.0.0.1:PORT") -> "http://127.0.0.1:PORT", so we pass
	// the bare host:port the httptest server exposes.
	host, port, _ := strings.Cut(strings.TrimPrefix(ts.URL, "http://"), ":")
	st := DefaultSettings()
	st.RouterAddr = host + ":" + port

	// The probe is a pure read (no StartWorker side effect), so a fresh
	// supervisor with no settings file and no worker binary is fine.
	s := newProbeSupervisor(t)

	got, err := s.routerProbe(context.Background(), st)
	if err != nil {
		t.Fatalf("defaultRouterProbe: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("probe used method %q, want GET", gotMethod)
	}
	if gotPath != "/v1/workers" {
		t.Errorf("probe used path %q, want /v1/workers", gotPath)
	}
	if len(got) != 1 {
		t.Fatalf("probe returned %d workers, want 1", len(got))
	}
	if got[0].ID != "probe-w-1" || got[0].Hostname != "probe-host" {
		t.Errorf("probe worker = %+v, want ID probe-w-1 / Hostname probe-host", got[0])
	}

	// The probe must not have started the worker as a side effect.
	if s.IsWorkerRunning() {
		t.Error("worker started by probe side effect, want not running")
	}
}

// TestDefaultRouterProbeInjectsTLSClient verifies the probe injects the
// Supervisor's mTLS transport when the router runs in prod mode. With a
// minted client cert the probe upgrades to https and cannot complete a TLS
// handshake against a plain-http mock — proving the transport was injected
// (issue #103 R1). A nil-supervisor client would hit the plain-http server
// and succeed, distinguishing the two paths.
func TestDefaultRouterProbeInjectsTLSClient(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(router.WorkersResponse{Workers: []router.WorkerInfo{}})
	}))
	defer ts.Close()

	// Provision a real CA + client cert so buildTLSClient can load them.
	caDir := t.TempDir()
	caCertPath, caKeyPath, err := security.GenerateSelfSignedCA(caDir)
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	clientDir := t.TempDir()
	clientCertPath, clientKeyPath, err := security.GenerateNodeCert(clientDir, caCertPath, caKeyPath, "infermesh-desktop")
	if err != nil {
		t.Fatalf("generate client cert: %v", err)
	}

	host, port, _ := strings.Cut(strings.TrimPrefix(ts.URL, "http://"), ":")
	st := DefaultSettings()
	st.RouterAddr = host + ":" + port

	s := newProbeSupervisor(t)
	// Simulate prod mode: the supervisor has minted a client cert and CA dir.
	s.clientCertPaths[0] = clientCertPath
	s.clientCertPaths[1] = clientKeyPath
	s.caCertDir = caDir

	_, err = s.routerProbe(context.Background(), st)
	if err == nil {
		t.Fatal("probe succeeded against plain-http mock with a prod-mode client cert; " +
			"expected the https upgrade (proving the TLS transport was injected)")
	}
	if !strings.Contains(err.Error(), "router unreachable") {
		t.Errorf("probe error %q should be the router-unreachable form from the https dial", err)
	}
}

// TestDefaultRouterProbeUnreachableRouter verifies a dead router surfaces as a
// plain error (the probe never panics) and no workers are returned — the
// "unreachable" half of the Status() contract.
func TestDefaultRouterProbeUnreachableRouter(t *testing.T) {
	st := DefaultSettings()
	st.RouterAddr = "127.0.0.1:1" // nothing listens here

	s := newProbeSupervisor(t)
	got, err := s.routerProbe(context.Background(), st)
	if err == nil {
		t.Fatal("probe against dead router: want error, got nil")
	}
	if got != nil {
		t.Errorf("probe workers = %+v, want nil on error", got)
	}
}

// TestNewSupervisorServiceWiresDefaultProbes documents that NewSupervisorService
// installs the default router/worker probes (the seams Status() drives), so a
// fresh supervisor can probe live services without extra wiring.
func TestNewSupervisorServiceWiresDefaultProbes(t *testing.T) {
	s := NewSupervisorService(nil, t.TempDir()+"/settings.yml", t.TempDir(), t.TempDir())
	if s.routerProbe == nil {
		t.Error("NewSupervisorService: routerProbe is nil")
	}
	if s.workerProbe == nil {
		t.Error("NewSupervisorService: workerProbe is nil")
	}
}
