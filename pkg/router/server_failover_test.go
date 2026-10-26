package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
	"github.com/seppaleinen/infermesh/pkg/scheduler"
)

func TestDispatchWithFailover_SuccessOnFirstAttempt(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-a", "failover-model"),
		mkWorker("worker-b", "failover-model"),
	}
	tr, srv, fc := failoverTestServer(t, workers)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-a"] = mockResult{
		body: []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"content":"success"}}]}`),
	}
	fc.results["worker-b"] = mockResult{
		body: []byte(`{"id":"chatcmpl-2","object":"chat.completion","choices":[{"message":{"content":"should-not-be-used"}}]}`),
	}

	body := []byte(`{"model":"failover-model","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := srv.dispatchWithFailover(context.Background(), "failover-model", body, "chat", 3, 10*time.Second)
	if err != nil {
		t.Fatalf("dispatchWithFailover failed: %v", err)
	}
	if !bytes.Contains(resp, []byte("success")) {
		t.Errorf("expected response from worker-a, got: %s", string(resp))
	}
	if fc.calls["worker-a"] != 1 {
		t.Errorf("expected 1 call to worker-a, got %d", fc.calls["worker-a"])
	}
	if fc.calls["worker-b"] != 0 {
		t.Errorf("expected 0 calls to worker-b, got %d", fc.calls["worker-b"])
	}
}
func TestDispatchWithFailover_SuccessOnSecondAttempt(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-a", "failover-model"),
		mkWorker("worker-b", "failover-model"),
	}
	tr, srv, fc := failoverTestServer(t, workers)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-a"] = mockResult{
		err: &WorkerHTTPError{StatusCode: 500, Body: "internal error", WorkerID: "worker-a"},
	}
	fc.results["worker-b"] = mockResult{
		body: []byte(`{"id":"chatcmpl-2","object":"chat.completion","choices":[{"message":{"content":"success"}}]}`),
	}

	body := []byte(`{"model":"failover-model","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := srv.dispatchWithFailover(context.Background(), "failover-model", body, "chat", 3, 10*time.Second)
	if err != nil {
		t.Fatalf("dispatchWithFailover failed: %v", err)
	}
	if !bytes.Contains(resp, []byte("success")) {
		t.Errorf("expected response from worker-b, got: %s", string(resp))
	}
	if fc.calls["worker-a"] != 1 {
		t.Errorf("expected 1 call to worker-a, got %d", fc.calls["worker-a"])
	}
	if fc.calls["worker-b"] != 1 {
		t.Errorf("expected 1 call to worker-b, got %d", fc.calls["worker-b"])
	}
}
func TestDispatchWithFailover_ExhaustedAttempts(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-a", "failover-model"),
		mkWorker("worker-b", "failover-model"),
	}
	tr, srv, fc := failoverTestServer(t, workers)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-a"] = mockResult{
		err: &WorkerHTTPError{StatusCode: 500, Body: "internal error", WorkerID: "worker-a"},
	}
	fc.results["worker-b"] = mockResult{
		err: &WorkerHTTPError{StatusCode: 500, Body: "internal error", WorkerID: "worker-b"},
	}

	body := []byte(`{"model":"failover-model","messages":[{"role":"user","content":"hi"}]}`)
	_, err := srv.dispatchWithFailover(context.Background(), "failover-model", body, "chat", 2, 10*time.Second)
	if err == nil {
		t.Fatal("expected error after exhausted attempts")
	}
	if fc.calls["worker-a"] != 1 || fc.calls["worker-b"] != 1 {
		t.Errorf("expected 1 call to each worker, got a=%d b=%d", fc.calls["worker-a"], fc.calls["worker-b"])
	}
}
func TestDispatchWithFailover_NonRetryableErrorStops(t *testing.T) {
	tr, srv, fc := failoverTestServer(t, []protocol.WorkerInfo{
		mkWorker("worker-a", "failover-model"),
		mkWorker("worker-b", "failover-model"),
	})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-a"] = mockResult{
		err: &WorkerHTTPError{StatusCode: 400, Body: "bad request", WorkerID: "worker-a"},
	}
	fc.results["worker-b"] = mockResult{
		body: []byte(`{"id":"chatcmpl-2","object":"chat.completion","choices":[{"message":{"content":"should-not-be-used"}}]}`),
	}

	body := []byte(`{"model":"failover-model","messages":[{"role":"user","content":"hi"}]}`)
	_, err := srv.dispatchWithFailover(context.Background(), "failover-model", body, "chat", 3, 10*time.Second)
	if err == nil {
		t.Fatal("expected error for non-retryable 400")
	}
	var herr *WorkerHTTPError
	if !errors.As(err, &herr) || herr.StatusCode != http.StatusBadRequest {
		t.Errorf("expected WorkerHTTPError 400, got %v", err)
	}
	if fc.calls["worker-a"] != 1 {
		t.Errorf("expected 1 call (fail-fast), got %d", fc.calls["worker-a"])
	}
	if fc.calls["worker-b"] != 0 {
		t.Errorf("expected 0 calls to worker-b, got %d", fc.calls["worker-b"])
	}
}
func TestDispatchWithFailover_WSWorkerBackendErrorFailover(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-a", "failover-model"),
		mkWorker("worker-b", "failover-model"),
	}
	tr, srv, fc := failoverTestServer(t, workers)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	// Simulate a WS worker backend error (e.g. LM Studio 404, model not loaded).
	fc.results["worker-a"] = mockResult{
		err: fmt.Errorf("%w: %w", errWorkerInferenceError, errors.New("model not loaded")),
	}
	fc.results["worker-b"] = mockResult{
		body: []byte(`{"id":"chatcmpl-2","object":"chat.completion","choices":[{"message":{"content":"success"}}]}`),
	}

	body := []byte(`{"model":"failover-model","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := srv.dispatchWithFailover(context.Background(), "failover-model", body, "chat", 3, 10*time.Second)
	if err != nil {
		t.Fatalf("dispatchWithFailover failed: %v", err)
	}
	if !bytes.Contains(resp, []byte("success")) {
		t.Errorf("expected response from worker-b, got: %s", string(resp))
	}
	if fc.calls["worker-a"] != 1 {
		t.Errorf("expected 1 call to worker-a, got %d", fc.calls["worker-a"])
	}
	if fc.calls["worker-b"] != 1 {
		t.Errorf("expected 1 call to worker-b, got %d", fc.calls["worker-b"])
	}
}
func TestDispatchWithFailover_WSWorkerBackendErrorExhausted(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-a", "failover-model"),
		mkWorker("worker-b", "failover-model"),
	}
	tr, srv, fc := failoverTestServer(t, workers)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	fc.results["worker-a"] = mockResult{
		err: fmt.Errorf("%w: %w", errWorkerInferenceError, errors.New("backend crashed")),
	}
	fc.results["worker-b"] = mockResult{
		err: fmt.Errorf("%w: %w", errWorkerInferenceError, errors.New("backend crashed")),
	}

	body := []byte(`{"model":"failover-model","messages":[{"role":"user","content":"hi"}]}`)
	_, err := srv.dispatchWithFailover(context.Background(), "failover-model", body, "chat", 2, 10*time.Second)
	if err == nil {
		t.Fatal("expected error after exhausted attempts")
	}
	if !errors.Is(err, errWorkerInferenceError) && !errors.Is(err, errAllAttemptsFailed) {
		t.Errorf("expected errWorkerInferenceError or errAllAttemptsFailed, got %v", err)
	}
	if fc.calls["worker-a"] != 1 || fc.calls["worker-b"] != 1 {
		t.Errorf("expected 1 call to each worker, got a=%d b=%d", fc.calls["worker-a"], fc.calls["worker-b"])
	}
}
func TestSelectWorkerExcluding_FiltersExcluded(t *testing.T) {
	workers := []protocol.WorkerInfo{
		mkWorker("worker-a", "failover-model"),
		mkWorker("worker-b", "failover-model"),
	}
	tr, srv, _ := failoverTestServer(t, workers)
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	// Exclude worker-a, should select worker-b
	w, err := srv.selectWorkerExcluding("failover-model", map[string]struct{}{"worker-a": {}})
	if err != nil {
		t.Fatalf("selectWorkerExcluding: %v", err)
	}
	if w.ID != "worker-b" {
		t.Errorf("expected worker-b, got %s", w.ID)
	}

	// Exclude both workers - should return error
	_, err = srv.selectWorkerExcluding("failover-model", map[string]struct{}{
		"worker-a": {},
		"worker-b": {},
	})
	if err == nil {
		t.Fatal("expected error when all workers excluded")
	}
	if !errors.Is(err, scheduler.ErrNoMatchingWorkers) {
		t.Errorf("expected ErrNoMatchingWorkers, got %v", err)
	}
}
func TestIsRetryableError_Taxonomy(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"timeout", errTimeout, true},
		{"connClosed", errConnClosed, true},
		{"relayDisconnected", errRelayDisconnected, true},
		{"rateLimited", errRateLimited, true},
		{"deadlineExceeded", context.DeadlineExceeded, true},
		{"worker500", &WorkerHTTPError{StatusCode: 500}, true},
		{"worker503", &WorkerHTTPError{StatusCode: 503}, true},
		{"worker429", &WorkerHTTPError{StatusCode: 429}, true},
		{"worker400", &WorkerHTTPError{StatusCode: 400}, false},
		{"worker404", &WorkerHTTPError{StatusCode: 404}, false},
		{"workerInferenceError", errWorkerInferenceError, true},
		{"workerInferenceError_wrapped", fmt.Errorf("%w: %w", errWorkerInferenceError, errors.New("model not found")), true},
		{"noMatchingWorkers", scheduler.ErrNoMatchingWorkers, false},
		{"allWorkersUnavailable", scheduler.ErrAllWorkersUnavailable, false},
		{"canceled", context.Canceled, false},
		{"parseError", fmt.Errorf("parse error"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableError(tt.err); got != tt.want {
				t.Errorf("isRetryableError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
func TestRetryBudget_DeadlineEnforced(t *testing.T) {
	tr, srv, fc := failoverTestServer(t, []protocol.WorkerInfo{
		mkWorker("worker-a", "budget-model"),
	})
	defer tr.cancel()
	defer func() { _ = tr.reg.Stop() }()

	// Mock worker that blocks until context is cancelled, then returns
	// context.DeadlineExceeded.
	fc.results["worker-a"] = mockResult{
		block: 200 * time.Millisecond,
	}

	body := []byte(`{"model":"budget-model","messages":[{"role":"user","content":"hi"}]}`)
	start := time.Now()
	_, err := srv.dispatchWithFailover(context.Background(), "budget-model", body, "chat", 3, 50*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error due to budget deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, errAllAttemptsFailed) {
		t.Errorf("expected deadline/timeout error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("budget not enforced: took %v", elapsed)
	}
}
