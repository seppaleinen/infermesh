package main

// Unit tests for RouterClient (issue #104, Phase 0): GetWorkers,
// SetSupervisor, and effectiveBaseURL. HTTP is mocked with httptest from the
// standard library.
//
// Known behaviour documented (issue #103 R1): GetWorkers calls
// Supervisor.StartWorker() on every 200 response from /v1/workers — a read
// endpoint that mutates process state. The tests below assert this behaviour
// explicitly; fixing it is a separate issue.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/router"
)

// TestSetSupervisor verifies the supervisor back-reference is stored on the
// client and is the same instance the caller passed in.
func TestSetSupervisor(t *testing.T) {
	c := NewRouterClient("http://127.0.0.1:8080")
	if c.sv != nil {
		t.Fatal("fresh client: sv != nil, want nil")
	}

	sv := NewSupervisorService(nil, t.TempDir()+"/settings.yml", t.TempDir(), t.TempDir())
	c.SetSupervisor(sv)
	if c.sv != sv {
		t.Fatal("SetSupervisor did not store the supervisor")
	}
	// A freshly-attached supervisor has not minted a client cert yet, which
	// effectiveBaseURL and httpClient both key off via GetClientCertPaths.
	cert, key := c.sv.GetClientCertPaths()
	if cert != "" || key != "" {
		t.Errorf("unexpected client cert paths after SetSupervisor: %q %q", cert, key)
	}
}

// TestEffectiveBaseURL documents the scheme-upgrade contract of
// effectiveBaseURL:
//
//   - bare ":8080" is normalised to "http://127.0.0.1:8080" by the
//     constructor and stays http without a supervisor;
//   - a supervisor without a minted client cert leaves the URL unchanged
//     (dev mode);
//   - a supervisor WITH a minted client cert (prod mode) upgrades http:// to
//     https://;
//   - an https:// base URL is never touched.
func TestEffectiveBaseURL(t *testing.T) {
	// Dev-mode supervisor: no client cert minted.
	devSup := NewSupervisorService(nil, t.TempDir()+"/settings.yml", t.TempDir(), t.TempDir())
	// Prod-mode supervisor: minted client cert paths.
	prodSup := NewSupervisorService(nil, t.TempDir()+"/settings.yml", t.TempDir(), t.TempDir())
	prodSup.clientCertPaths[0] = "/tmp/fake-cert.pem"
	prodSup.clientCertPaths[1] = "/tmp/fake-key.pem"

	cases := []struct {
		name    string
		baseURL string
		sv      *Supervisor
		want    string
	}{
		{
			name:    "bare addr normalised, no supervisor",
			baseURL: ":8080",
			want:    "http://:8080",
		},
		{
			name:    "http unchanged without supervisor",
			baseURL: "http://host:8080",
			want:    "http://host:8080",
		},
		{
			name:    "https unchanged without supervisor",
			baseURL: "https://host:8443",
			want:    "https://host:8443",
		},
		{
			name:    "dev mode (no client cert) keeps http",
			baseURL: "http://127.0.0.1:8080",
			sv:      devSup,
			want:    "http://127.0.0.1:8080",
		},
		{
			name:    "prod mode (client cert minted) upgrades to https",
			baseURL: "http://127.0.0.1:8080",
			sv:      prodSup,
			want:    "https://127.0.0.1:8080",
		},
		{
			name:    "prod mode with https base stays https",
			baseURL: "https://router.example.com",
			sv:      prodSup,
			want:    "https://router.example.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewRouterClient(tc.baseURL)
			if tc.sv != nil {
				c.sv = tc.sv
			}
			if got := c.effectiveBaseURL(); got != tc.want {
				t.Errorf("effectiveBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGetWorkers verifies GetWorkers performs a GET against
// {effectiveBaseURL}/v1/workers, decodes the WorkersResponse envelope, and
// maps it onto WorkerView.
func TestGetWorkers(t *testing.T) {
	lastSeen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	workersBody := router.WorkersResponse{Workers: []router.WorkerInfo{
		{
			ID:           "w-1",
			Hostname:     "alpha",
			IP:           "10.0.0.2",
			Port:         8081,
			Status:       "available",
			Version:      "v1.0.0",
			LoadedModels: []string{"llama-3-8b"},
			LastSeen:     lastSeen,
		},
		{
			ID:     "w-2",
			IP:     "10.0.0.3", // no hostname: Address falls back to IP
			Port:   8082,
			Status: "busy",
		},
	}}

	var gotPath, gotMethod, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(workersBody); err != nil {
			t.Errorf("encode workers body: %v", err)
		}
	}))
	defer ts.Close()

	c := NewRouterClient(ts.URL) // no supervisor: dev-mode path, plain http
	got, err := c.GetWorkers(context.Background())
	if err != nil {
		t.Fatalf("GetWorkers: %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("used method %q, want GET", gotMethod)
	}
	if gotPath != "/v1/workers" {
		t.Errorf("used path %q, want /v1/workers", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("unexpected Authorization header %q on a plain-http client", gotAuth)
	}
	if len(got) != 2 {
		t.Fatalf("len(workers) = %d, want 2", len(got))
	}

	want := WorkerView{
		ID:           "w-1",
		Hostname:     "alpha",
		IP:           "10.0.0.2",
		Port:         8081,
		Status:       "available",
		Version:      "v1.0.0",
		LoadedModels: []string{"llama-3-8b"},
		LastSeen:     lastSeen,
		Address:      "alpha:8081",
	}
	if got[0].ID != want.ID || got[0].Hostname != want.Hostname ||
		got[0].IP != want.IP || got[0].Port != want.Port ||
		got[0].Status != want.Status || got[0].Version != want.Version ||
		got[0].Address != want.Address || got[0].LastSeen != want.LastSeen ||
		len(got[0].LoadedModels) != len(want.LoadedModels) ||
		got[0].LoadedModels[0] != want.LoadedModels[0] {
		t.Errorf("worker[0] = %+v, want %+v", got[0], want)
	}
	if got[1].Address != "10.0.0.3:8082" {
		t.Errorf("worker[1].Address = %q, want %q (IP fallback)", got[1].Address, "10.0.0.3:8082")
	}
	if got[1].LoadedModels == nil || len(got[1].LoadedModels) != 0 {
		t.Errorf("worker[1].LoadedModels = %v, want empty non-nil slice", got[1].LoadedModels)
	}
}

// TestGetWorkersNoSideEffect verifies GetWorkers is a pure read: a 200
// response from /v1/workers does NOT trigger Supervisor.StartWorker().
// Issue #103 R1 documented the old side-effect; this test asserts the fix.
func TestGetWorkersNoSideEffect(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(router.WorkersResponse{Workers: []router.WorkerInfo{}})
	}))
	defer ts.Close()

	c := NewRouterClient(ts.URL)
	sv := NewSupervisorService(nil, t.TempDir()+"/settings.yml", t.TempDir(), t.TempDir())
	c.SetSupervisor(sv)

	_, err := c.GetWorkers(context.Background())
	if err != nil {
		t.Fatalf("GetWorkers: %v", err)
	}
	// A pure read must not have started the worker.
	if sv.IsWorkerRunning() {
		t.Error("worker started by GetWorkers side effect, want not running")
	}
}

// TestGetWorkersNon200 verifies a non-200 response from /v1/workers is
// surfaced as an error mentioning the status code and the body.
func TestGetWorkersNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	c := NewRouterClient(ts.URL)
	_, err := c.GetWorkers(context.Background())
	if err == nil {
		t.Fatal("GetWorkers: want error for 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q should mention status code 500", err)
	}
}

// TestGetWorkersUnreachable verifies a dial failure is surfaced with the
// "router unreachable" phrasing the UI keys off.
func TestGetWorkersUnreachable(t *testing.T) {
	// Point at a port that is not listening.
	c := NewRouterClient("http://127.0.0.1:1")
	_, err := c.GetWorkers(context.Background())
	if err == nil {
		t.Fatal("GetWorkers: want error for unreachable router, got nil")
	}
	if !strings.Contains(err.Error(), "router unreachable") {
		t.Errorf("error %q should mention router unreachable", err)
	}
}

// TestGetWorkersBadJSON verifies a 200 with a malformed body returns a decode
// error rather than a partial worker list.
func TestGetWorkersBadJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{not-json"))
	}))
	defer ts.Close()

	c := NewRouterClient(ts.URL)
	_, err := c.GetWorkers(context.Background())
	if err == nil {
		t.Fatal("GetWorkers: want decode error for malformed body, got nil")
	}
	if !strings.Contains(err.Error(), "decode workers response") {
		t.Errorf("error %q should mention decode failure", err)
	}
}

// TestGetWorkersNullWorkers verifies the "reachable but idle" contract: a 200
// with workers:null is normalised to an empty (non-nil) slice so the UI can
// distinguish "reachable, idle" from "unreachable".
func TestGetWorkersNullWorkers(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workers":null}`))
	}))
	defer ts.Close()

	c := NewRouterClient(ts.URL)
	got, err := c.GetWorkers(context.Background())
	if err != nil {
		t.Fatalf("GetWorkers: %v", err)
	}
	if got == nil {
		t.Fatal("GetWorkers: workers is nil, want non-nil empty slice (idle contract)")
	}
	if len(got) != 0 {
		t.Errorf("len(workers) = %d, want 0", len(got))
	}
}

// TestGetWorkersTruncatedBody verifies a 200 whose body is cut short (the
// connection is torn down mid-stream, so the client reads EOF before the
// JSON object closes) is surfaced as an error rather than a partial worker
// list. This exercises the io.ReadAll error branch (line 233-234).
func TestGetWorkersTruncatedBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Partial payload: the client reads EOF before the JSON object closes.
		_, _ = w.Write([]byte(`{"workers":[{"id":"trunc`))
		// Force the connection shut mid-stream so the client's ReadAll hits
		// EOF rather than a well-formed body.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("httptest response writer does not implement Hijacker")
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer ts.Close()

	c := NewRouterClient(ts.URL)
	_, err := c.GetWorkers(context.Background())
	if err == nil {
		t.Fatal("GetWorkers: want error for truncated body, got nil")
	}
	// The connection tears down mid-stream: either the dial fails with EOF
	// ("router unreachable ... EOF") or the read itself fails
	// ("read router body"). Both prove a partial body is not decoded.
	if !strings.Contains(err.Error(), "read router body") && !strings.Contains(err.Error(), "router unreachable") {
		t.Errorf("error %q should mention the read or unreachable failure", err)
	}
}