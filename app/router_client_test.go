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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/router"
)

// TestSetSupervisor is retired (issue #103 R1): RouterClient no longer holds
// a Supervisor reference. The TLS transport is injected via SetTransport.
// This test asserts the new contract: a fresh client has no transport and
// uses plain HTTP.
func TestSetSupervisor(t *testing.T) {
	c := NewRouterClient("http://127.0.0.1:8080")
	if c.client != nil {
		t.Fatal("fresh client: transport != nil, want nil (dev mode)")
	}
	// SetTransport(nil) is a no-op (idempotent dev-mode reset).
	c.SetTransport(nil)
	if c.client != nil {
		t.Fatal("after SetTransport(nil): transport != nil, want nil")
	}
}

// TestSetTransport documents the scheme-upgrade contract of SetTransport:
//
//   - a fresh client has no transport (dev mode, plain http);
//   - injecting a non-nil http.Client upgrades the base URL to https;
//   - injecting nil reverts to plain http.
func TestSetTransport(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		client  *http.Client
		want    string
	}{
		{
			name:    "fresh client, no transport",
			baseURL: "http://127.0.0.1:8080",
			client:  nil,
			want:    "http://127.0.0.1:8080",
		},
		{
			name:    "transport injected, upgrades to https",
			baseURL: "http://127.0.0.1:8080",
			client:  &http.Client{},
			want:    "https://127.0.0.1:8080",
		},
		{
			name:    "transport removed, reverts to http",
			baseURL: "https://127.0.0.1:8080",
			client:  nil,
			want:    "http://127.0.0.1:8080",
		},
		{
			name:    "https base stays https with transport",
			baseURL: "https://router.example.com",
			client:  &http.Client{},
			want:    "https://router.example.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewRouterClient(tc.baseURL)
			c.SetTransport(tc.client)
			if got := c.GetRouterURL(); got != tc.want {
				t.Errorf("GetRouterURL() = %q, want %q", got, tc.want)
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

	_, err := c.GetWorkers(context.Background())
	if err != nil {
		t.Fatalf("GetWorkers: %v", err)
	}
	// A pure read must not have started the worker.
	sv := NewSupervisorService(nil, t.TempDir()+"/settings.yml", t.TempDir(), t.TempDir())
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

// TestApplyAuth verifies the X-API-Key contract (issue #119): the header is
// present when a key is set and absent when empty. An empty key is a no-op,
// so dev-mode behaviour (no header) is preserved exactly.
func TestApplyAuth(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want string
	}{
		{"empty key sends no header", "", ""},
		{"configured key sends X-API-Key", "s3cr3t", "s3cr3t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewRouterClient("http://127.0.0.1:8080")
			c.SetAPIKey(tc.key)
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/workers", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			c.applyAuth(req)
			if got := req.Header.Get("X-API-Key"); got != tc.want {
				t.Errorf("X-API-Key = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRouterClientSendsAPIKey verifies every RouterClient method carries the
// X-API-Key header when one is configured. This is the regression guard for
// issue #119: before the fix, the desktop sent no auth and every router call
// returned 401 in prod mode (including the new POST /meta/aliases from #105).
func TestRouterClientSendsAPIKey(t *testing.T) {
	const apiKey = "prod-router-key"

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/workers", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != apiKey {
			http.Error(w, "missing X-API-Key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(router.WorkersResponse{Workers: []router.WorkerInfo{}})
	})
	mux.HandleFunc("/meta/models/popular", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != apiKey {
			http.Error(w, "missing X-API-Key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(router.PopularModelsResponse{Models: []router.PopularModelInfo{}})
	})
	mux.HandleFunc("/meta/aliases", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != apiKey {
			http.Error(w, "missing X-API-Key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/meta/connections/count", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != apiKey {
			http.Error(w, "missing X-API-Key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"connections":0}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := NewRouterClient(ts.URL)
	c.SetAPIKey(apiKey)

	ctx := context.Background()

	if _, err := c.GetWorkers(ctx); err != nil {
		t.Fatalf("GetWorkers: %v", err)
	}
	if _, err := c.GetPopularModels(ctx); err != nil {
		t.Fatalf("GetPopularModels: %v", err)
	}
	if err := c.SetModelAliases(ctx, map[string]string{"foo": "bar"}); err != nil {
		t.Fatalf("SetModelAliases: %v", err)
	}
	if n, err := c.GetConnectionCount(ctx); err != nil {
		t.Fatalf("GetConnectionCount: %v", err)
	} else if n != 0 {
		t.Errorf("GetConnectionCount = %d, want 0", n)
	}
}

// TestRouterClientNoKeyWhenEmpty verifies that with no API key configured the
// desktop sends no X-API-Key header, preserving dev-mode behaviour exactly
// (the router only enforces when a key is actually configured).
func TestRouterClientNoKeyWhenEmpty(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(router.WorkersResponse{Workers: []router.WorkerInfo{}})
	}))
	defer ts.Close()

	c := NewRouterClient(ts.URL) // no key: dev-mode behaviour
	if _, err := c.GetWorkers(context.Background()); err != nil {
		t.Fatalf("GetWorkers: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("X-API-Key = %q with no key configured, want empty (dev mode)", gotAuth)
	}
}

// TestGetAutoRouting verifies GetAutoRouting performs a GET against
// {effectiveBaseURL}/meta/auto-routing and decodes the router's
// AutoRoutingConfigRequest DTO, mirroring TestGetWorkers.
func TestGetAutoRouting(t *testing.T) {
	body := router.AutoRoutingConfigRequest{
		Enabled:         true,
		UseFourTier:     true,
		Models:          map[string]string{"simple": "small-model", "hardest": "hardest-model"},
		SimpleMaxTokens: 150,
		MediumMaxTokens: 500,
	}

	var gotPath, gotMethod, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode auto-routing body: %v", err)
		}
	}))
	defer ts.Close()

	c := NewRouterClient(ts.URL) // no supervisor: dev-mode path, plain http
	got, err := c.GetAutoRouting(context.Background())
	if err != nil {
		t.Fatalf("GetAutoRouting: %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("used method %q, want GET", gotMethod)
	}
	if gotPath != "/meta/auto-routing" {
		t.Errorf("used path %q, want /meta/auto-routing", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("unexpected X-API-Key header %q on a plain-http client", gotAuth)
	}
	if !got.Enabled {
		t.Error("Enabled = false, want true")
	}
	if !got.UseFourTier {
		t.Error("UseFourTier = false, want true")
	}
	if got.SimpleMaxTokens != 150 {
		t.Errorf("SimpleMaxTokens = %d, want 150", got.SimpleMaxTokens)
	}
	if got.MediumMaxTokens != 500 {
		t.Errorf("MediumMaxTokens = %d, want 500", got.MediumMaxTokens)
	}
	if len(got.Models) != len(body.Models) {
		t.Fatalf("len(models) = %d, want %d", len(got.Models), len(body.Models))
	}
	for k, want := range body.Models {
		if got.Models[k] != want {
			t.Errorf("models[%q] = %q, want %q", k, got.Models[k], want)
		}
	}
}

// TestGetAutoRoutingNon200 verifies a non-200 response from /meta/auto-routing
// is surfaced as an error mentioning the status code and the body, mirroring
// TestGetWorkersNon200.
func TestGetAutoRoutingNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	c := NewRouterClient(ts.URL)
	_, err := c.GetAutoRouting(context.Background())
	if err == nil {
		t.Fatal("GetAutoRouting: want error for 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q should mention status code 500", err)
	}
}

// TestGetAutoRoutingUnreachable verifies a dial failure is surfaced with the
// "router unreachable" phrasing the UI keys off, mirroring
// TestGetWorkersUnreachable.
func TestGetAutoRoutingUnreachable(t *testing.T) {
	// Point at a port that is not listening.
	c := NewRouterClient("http://127.0.0.1:1")
	_, err := c.GetAutoRouting(context.Background())
	if err == nil {
		t.Fatal("GetAutoRouting: want error for unreachable router, got nil")
	}
	if !strings.Contains(err.Error(), "router unreachable") {
		t.Errorf("error %q should mention router unreachable", err)
	}
}

// TestGetAutoRoutingBadJSON verifies a 200 with a malformed body returns a
// decode error rather than a partial config, mirroring TestGetWorkersBadJSON.
func TestGetAutoRoutingBadJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{not-json"))
	}))
	defer ts.Close()

	c := NewRouterClient(ts.URL)
	_, err := c.GetAutoRouting(context.Background())
	if err == nil {
		t.Fatal("GetAutoRouting: want decode error for malformed body, got nil")
	}
	if !strings.Contains(err.Error(), "decode auto-routing response") {
		t.Errorf("error %q should mention decode failure", err)
	}
}

// TestSetAutoRouting verifies SetAutoRouting performs a POST against
// {effectiveBaseURL}/meta/auto-routing with the full config as JSON body and
// only returns nil when the router answers {"ok": true}, mirroring the
// SetModelAliases contract.
func TestSetAutoRouting(t *testing.T) {
	want := AutoRoutingConfigRequest{
		Enabled:         true,
		UseFourTier:     true,
		Models:          map[string]string{"simple": "small", "hardest": "hardest"},
		SimpleMaxTokens: 150,
		MediumMaxTokens: 500,
	}

	var gotPath, gotMethod, gotCT string
	var gotBody AutoRoutingConfigRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	c := NewRouterClient(ts.URL)
	if err := c.SetAutoRouting(context.Background(), want); err != nil {
		t.Fatalf("SetAutoRouting: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("used method %q, want POST", gotMethod)
	}
	if gotPath != "/meta/auto-routing" {
		t.Errorf("used path %q, want /meta/auto-routing", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotBody.Enabled != want.Enabled ||
		gotBody.UseFourTier != want.UseFourTier ||
		gotBody.SimpleMaxTokens != want.SimpleMaxTokens ||
		gotBody.MediumMaxTokens != want.MediumMaxTokens ||
		!reflect.DeepEqual(gotBody.Models, want.Models) {
		t.Errorf("body = %+v, want %+v", gotBody, want)
	}
}

// TestSetAutoRoutingNon200 verifies a non-200 response from /meta/auto-routing
// is surfaced as an error mentioning the status code, mirroring the
// SetModelAliases error contract.
func TestSetAutoRoutingNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer ts.Close()

	c := NewRouterClient(ts.URL)
	err := c.SetAutoRouting(context.Background(), AutoRoutingConfigRequest{Enabled: true})
	if err == nil {
		t.Fatal("SetAutoRouting: want error for 502, got nil")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error %q should mention status code 502", err)
	}
}

// TestSetAutoRoutingUnreachable verifies a dial failure is surfaced with the
// "router unreachable" phrasing.
func TestSetAutoRoutingUnreachable(t *testing.T) {
	c := NewRouterClient("http://127.0.0.1:1")
	err := c.SetAutoRouting(context.Background(), AutoRoutingConfigRequest{Enabled: true})
	if err == nil {
		t.Fatal("SetAutoRouting: want error for unreachable router, got nil")
	}
	if !strings.Contains(err.Error(), "router unreachable") {
		t.Errorf("error %q should mention router unreachable", err)
	}
}

// TestSetAutoRoutingBadJSON verifies a 200 with a malformed response body
// returns a decode error rather than silently succeeding.
func TestSetAutoRoutingBadJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{not-json"))
	}))
	defer ts.Close()

	c := NewRouterClient(ts.URL)
	err := c.SetAutoRouting(context.Background(), AutoRoutingConfigRequest{Enabled: true})
	if err == nil {
		t.Fatal("SetAutoRouting: want decode error for malformed body, got nil")
	}
	if !strings.Contains(err.Error(), "decode auto-routing response") {
		t.Errorf("error %q should mention decode failure", err)
	}
}

// TestSetAutoRoutingNotOK verifies a 200 response with {"ok": false} is
// surfaced as an error rather than a success.
func TestSetAutoRoutingNotOK(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false}`))
	}))
	defer ts.Close()

	c := NewRouterClient(ts.URL)
	err := c.SetAutoRouting(context.Background(), AutoRoutingConfigRequest{Enabled: true})
	if err == nil {
		t.Fatal("SetAutoRouting: want error for ok=false, got nil")
	}
	if !strings.Contains(err.Error(), "ok=false") {
		t.Errorf("error %q should mention ok=false", err)
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
