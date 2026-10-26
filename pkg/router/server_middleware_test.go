package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/seppaleinen/infermesh/pkg/security"
)

func TestWrapInferenceEndpointsDevModeAPIKey(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	// Dev mode with an api-key configured.
	server := tr.ServerWithCfg(security.Config{DevMode: true, APIKey: "heybaby"})

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
	wrapped := server.wrapInferenceEndpoints(next, "heybaby")

	tests := []struct {
		name           string
		path           string
		header         string
		expectedStatus int
	}{
		{"inference endpoint missing key", "/v1/chat/completions", "", http.StatusUnauthorized},
		{"inference endpoint wrong key", "/v1/chat/completions", "wrong", http.StatusUnauthorized},
		{"inference endpoint valid key", "/v1/chat/completions", "heybaby", http.StatusOK},
		{"completion endpoint valid key", "/v1/completions", "heybaby", http.StatusOK},
		{"non-inference endpoint passes through", "/v1/models", "", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, nil)
			if tt.header != "" {
				req.Header.Set("X-API-Key", tt.header)
			}
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, req)
			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d, body: %s", tt.expectedStatus, w.Code, w.Body.String())
			}
		})
	}
}
func TestWrapInferenceEndpointsNoKeyDevMode(t *testing.T) {
	tr := testRegistry(t, nil)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	server := tr.Server() // dev mode, no api-key
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := server.wrapInferenceEndpoints(next, "")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected pass-through 200, got %d", w.Code)
	}
}
