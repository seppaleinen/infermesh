package security

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
)

// AuditLogger logs security audit events.
type AuditLogger interface {
	Log(ctx context.Context, message string, attrs map[string]interface{})
}

// fileAuditLogger writes JSON-lines to a file.
type fileAuditLogger struct {
	path string
	mu   sync.Mutex
	file *os.File
}

// NewFileAuditLogger creates a file-based audit logger.
func NewFileAuditLogger(path string) (AuditLogger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open audit log file: %w", err)
	}
	return &fileAuditLogger{path: path, file: f}, nil
}

// Log writes a JSON-line audit entry.
func (l *fileAuditLogger) Log(ctx context.Context, message string, attrs map[string]interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return
	}

	entry := map[string]interface{}{
		"message": message,
	}
	for k, v := range attrs {
		entry[k] = v
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}

	_, _ = fmt.Fprintln(l.file, string(data))
}

// NewMTLSMiddleware creates an mTLS middleware.
// In dev mode (required=false): passes through without inspection.
// In prod mode (required=true): checks tls.ConnectionState client cert.
func NewMTLSMiddleware(caCertPool *x509.CertPool, required bool, allowedCN ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !required {
				next.ServeHTTP(w, r)
				return
			}

			// Prod mode: check client certificate
			tlsState := r.TLS
			if tlsState == nil || len(tlsState.PeerCertificates) == 0 {
				logAudit(r.Context(), "mTLS auth denied: no client certificate", r)
				http.Error(w, "client certificate required", http.StatusForbidden)
				return
			}

			clientCert := tlsState.PeerCertificates[0]

			// Verify chain against CA pool
			opts := x509.VerifyOptions{
				Roots: caCertPool,
			}
			if _, err := clientCert.Verify(opts); err != nil {
				logAudit(r.Context(), "mTLS auth denied: certificate verification failed", r)
				http.Error(w, "certificate verification failed", http.StatusForbidden)
				return
			}

			// Check allowed CN if specified
			if len(allowedCN) > 0 {
				matched := false
				for _, cn := range allowedCN {
					if strings.Contains(clientCert.Subject.CommonName, cn) {
						matched = true
						break
					}
				}
				if !matched {
					logAudit(r.Context(), "mTLS auth denied: CN not allowed", r)
					http.Error(w, "common name not allowed", http.StatusForbidden)
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

// NewAPIKeyMiddleware creates a middleware that enforces API key authentication
// on inference endpoints. When required is false (dev mode) it passes through.
// When required is true every request must present a valid key in the
// X-API-Key header.
func NewAPIKeyMiddleware(apiKey string, required bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !required {
				next.ServeHTTP(w, r)
				return
			}
			key := r.Header.Get("X-API-Key")
			if key == "" {
				logAudit(r.Context(), "API key denied: missing X-API-Key header", r)
				http.Error(w, "API key required", http.StatusUnauthorized)
				return
			}
			if key != apiKey {
				logAudit(r.Context(), "API key denied: invalid key", r)
				http.Error(w, "invalid API key", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SplitCNs splits a comma-separated list of trusted CNs into a slice.
// An empty string yields an empty slice (no CN restriction).
func SplitCNs(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func logAudit(ctx context.Context, msg string, r *http.Request) {
	slog.Warn(msg, "remote_addr", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
}