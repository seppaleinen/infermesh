package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/seppaleinen/infermesh/pkg/security"
)

// TestHandleSetAliases tests the POST /meta/aliases handler.
func TestHandleSetAliases(t *testing.T) {
	// Each test creates a fresh server to avoid alias leakage between tests
	tests := []struct {
		name       string
		method     string
		body       interface{}
		wantStatus int
	}{
		{
			name:   "valid aliases",
			method: http.MethodPost,
			body: SetAliasesRequest{
				Aliases: map[string]string{
					"gemma-4b": "google/gemma-4b",
					"qwen-c":   "qwen2.5-coder-7b",
				},
			},
			wantStatus: http.StatusOK,
		},
		{
			name:   "empty map disables aliasing",
			method: http.MethodPost,
			body:   SetAliasesRequest{Aliases: map[string]string{}},
			wantStatus: http.StatusOK,
		},
		{
			name:   "empty canonical disables aliasing",
			method: http.MethodPost,
			body: SetAliasesRequest{
				Aliases: map[string]string{
					"alias1": "",
					"alias2": "   ",
				},
			},
			wantStatus: http.StatusOK,
		},
		{
			name:       "method not allowed",
			method:     http.MethodGet,
			body:       nil,
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "invalid json",
			method:     http.MethodPost,
			body:       "not json",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewServer(nil, testLogger(), ":0", security.Config{DevMode: true})
			
			var reqBody []byte
			var err error
			if tt.body != nil {
				// If body is a string, marshal it directly
				if s, ok := tt.body.(string); ok {
					reqBody = []byte(s)
				} else {
					reqBody, err = json.Marshal(tt.body)
					if err != nil {
						t.Fatalf("failed to marshal body: %v", err)
					}
				}
			} else if tt.name == "invalid json" {
				reqBody = []byte("not json")
			}

			req := httptest.NewRequest(tt.method, "/meta/aliases", bytes.NewReader(reqBody))
			w := httptest.NewRecorder()

			server.handleSetAliases(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %v, want %v", w.Code, tt.wantStatus)
			}

			// Verify specific behavior for each test
			switch tt.name {
			case "valid aliases":
				if !server.modelAliasesActive() {
					t.Errorf("model aliases should be active")
				}
				if server.modelAliases.Aliases["gemma-4b"].Canonical != "google/gemma-4b" {
					t.Errorf("wrong canonical for gemma-4b")
				}
				resolved := server.resolveAliasModel("gemma-4b")
				if resolved != "google/gemma-4b" {
					t.Errorf("resolveAliasModel returned %q, want %q", resolved, "google/gemma-4b")
				}
			case "empty map disables aliasing":
				if server.modelAliasesActive() {
					t.Errorf("model aliases should be disabled for empty map")
				}
				if server.modelAliases != nil {
					t.Errorf("modelAliases should be nil")
				}
			case "empty canonical disables aliasing":
				if server.modelAliasesActive() {
					t.Errorf("model aliases should be disabled when all canonical are empty")
				}
			}
		})
	}
}

// TestBuildAliasConfig tests the buildAliasConfig helper directly.
func TestBuildAliasConfig(t *testing.T) {
	tests := []struct {
		name          string
		raw           map[string]string
		wantNil       bool
		wantCount     int
		wantCanonical []string
	}{
		{
			name:      "nil map",
			raw:       nil,
			wantNil:   true,
			wantCount: 0,
		},
		{
			name:      "empty map",
			raw:       map[string]string{},
			wantNil:   true,
			wantCount: 0,
		},
		{
			name: "valid entries",
			raw: map[string]string{
				"a": "canonical-a",
				"b": "canonical-b",
			},
			wantNil:   false,
			wantCount: 2,
		},
		{
			name: "skip empty canonical",
			raw: map[string]string{
				"a": "canonical-a",
				"b": "",
				"c": "   ",
			},
			wantNil:   false,
			wantCount: 1,
		},
		{
			name: "all empty canonical",
			raw: map[string]string{
				"a": "",
				"b": " ",
			},
			wantNil:   true,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := buildAliasConfig(tt.raw)
			if tt.wantNil {
				if cfg != nil {
					t.Errorf("expected nil config, got %+v", cfg)
				}
				return
			}
			if cfg == nil {
				t.Fatalf("expected non-nil config")
			}
			if len(cfg.Aliases) != tt.wantCount {
				t.Errorf("expected %d aliases, got %d", tt.wantCount, len(cfg.Aliases))
			}
		})
	}
}
