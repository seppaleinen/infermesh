package router

import (
    "bytes"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "testing"

    "github.com/seppaleinen/infermesh/pkg/protocol"
)

// TestDevRegisterDuplicateIDFromDifferentPeer verifies that a second worker
// attempting to register with a worker ID already claimed by a different
// peer IP is rejected with a 409 Conflict.
func TestDevRegisterDuplicateIDFromDifferentPeer(t *testing.T) {
    // Use existing helper to create test registry and server.
    tr := testRegistry(t, nil)
    defer tr.cancel()
    defer func() { _ = tr.reg.Stop() }()
    server := tr.Server()

    workerID := "dup-worker"
    worker := protocol.WorkerInfo{
        ID:   workerID,
        IP:   "192.168.1.100",
        Port: 8081,
    }
    body, _ := json.Marshal(worker)

    // First registration from peer 1
    req1 := httptest.NewRequest(http.MethodPost, "/v1/dev/register", bytes.NewReader(body))
    req1.Header.Set("Content-Type", "application/json")
    req1.RemoteAddr = "192.168.1.10:50000"
    w1 := httptest.NewRecorder()
    server.handleDevRegister(w1, req1)
    if w1.Code != http.StatusOK {
        t.Fatalf("first register failed: %d %s", w1.Code, w1.Body.String())
    }

    // Second registration from a different peer with same ID
    req2 := httptest.NewRequest(http.MethodPost, "/v1/dev/register", bytes.NewReader(body))
    req2.Header.Set("Content-Type", "application/json")
    req2.RemoteAddr = "192.168.1.20:50000"
    w2 := httptest.NewRecorder()
    server.handleDevRegister(w2, req2)
    if w2.Code != http.StatusConflict {
        t.Fatalf("expected conflict, got %d %s", w2.Code, w2.Body.String())
    }
    // Ensure the registry still contains the original worker
    got, ok := tr.reg.Get(workerID)
    if !ok {
        t.Fatal("worker not present after collision attempt")
    }
    if got.IP != "192.168.1.10" {
        t.Fatalf("worker IP modified by collision attempt, got %s", got.IP)
    }
}
