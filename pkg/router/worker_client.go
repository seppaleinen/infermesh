package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// StreamEvent is a single unit of a proxied inference stream.
// Data carries the raw chunk bytes (an SSE frame for HTTP workers, a raw
// JSON response/chunk for WebSocket workers); Done marks the terminal event.
type StreamEvent struct {
	Data []byte
	Done bool
}

// WorkerClient is the transport-agnostic way the router talks to a worker
// for inference, model loading and capability fetching.
//
// Body is an OpenAI-compatible request body; kind is "chat" or "completion".
type WorkerClient interface {
	// Transport returns the transport this client uses: "http" or "ws".
	Transport() string
	// Complete sends a non-streaming inference request and returns the
	// worker's full response body.
	Complete(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) ([]byte, error)
	// Stream sends an inference request and returns a channel of stream
	// events. The channel is closed after the terminal event; transport
	// errors arrive on the error channel.
	Stream(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) (<-chan StreamEvent, <-chan error)
	// LoadModel asks the worker to load a model.
	LoadModel(ctx context.Context, worker protocol.WorkerInfo, model string) (bool, error)
	// Capabilities fetches the worker's capability snapshot.
	Capabilities(ctx context.Context, worker protocol.WorkerInfo) (protocol.Capabilities, error)
	// Close releases the client (no-op for HTTP clients).
	Close() error
}

// WorkerHTTPError carries a non-200 response from a worker HTTP endpoint so
// callers can map it back to a client-facing status code.
type WorkerHTTPError struct {
	StatusCode int
	Body       string
	WorkerID   string
}

func (e *WorkerHTTPError) Error() string {
	return fmt.Sprintf("worker %s returned HTTP %d: %s", e.WorkerID, e.StatusCode, truncate(e.Body, 256))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// httpWorkerClient proxies to a worker's legacy HTTP API. It is the
// dial-back path used by mDNS/discovered and dev-HTTP workers.
type httpWorkerClient struct {
	log *slog.Logger
}

func newHTTPWorkerClient(log *slog.Logger) *httpWorkerClient {
	return &httpWorkerClient{log: log}
}

func (c *httpWorkerClient) Transport() string { return protocol.TransportHTTP }

// endpoint returns the worker HTTP URL for the given kind.
func endpoint(worker protocol.WorkerInfo, kind string) string {
	base := fmt.Sprintf("http://%s:%d", worker.IP, worker.Port)
	if kind == "completion" {
		return base + "/v1/completions"
	}
	return base + "/v1/chat/completions"
}

// postWithRetry POSTs body to url with exponential backoff (max 3 retries).
func postWithRetry(ctx context.Context, client *http.Client, url string, body []byte) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if attempt < 3 {
			time.Sleep(time.Duration(1<<attempt) * time.Second)
		}
	}
	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

func (c *httpWorkerClient) Complete(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := postWithRetry(ctx, client, endpoint(worker, kind), body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, &WorkerHTTPError{StatusCode: resp.StatusCode, Body: string(raw), WorkerID: worker.ID}
	}
	return io.ReadAll(resp.Body)
}

func (c *httpWorkerClient) Stream(ctx context.Context, worker protocol.WorkerInfo, kind string, body []byte) (<-chan StreamEvent, <-chan error) {
	chunkCh := make(chan StreamEvent, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(chunkCh)

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := postWithRetry(ctx, client, endpoint(worker, kind), body)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				errCh <- fmt.Errorf("request timed out after retries: %w", ctx.Err())
			} else {
				errCh <- fmt.Errorf("failed to connect to worker after retries: %w", err)
			}
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			errCh <- &WorkerHTTPError{StatusCode: resp.StatusCode, Body: string(raw), WorkerID: worker.ID}
			return
		}

		// Relay the worker's SSE stream (data: {...}\n\n frames) verbatim.
		reader := bufio.NewReader(resp.Body)
		for {
			frame, err := readSSEFrame(reader)
			if len(frame) > 0 {
				select {
				case chunkCh <- StreamEvent{Data: frame}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && ctx.Err() == nil {
					errCh <- fmt.Errorf("read SSE frame: %w", err)
				}
				return
			}
		}
	}()

	return chunkCh, errCh
}

// readSSEFrame reads one Server-Sent-Events frame: a run of "field: value"
// lines terminated by a blank line. The returned slice includes the trailing
// blank line so relaying it verbatim preserves the wire format.
func readSSEFrame(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			buf = append(buf, line...)
			if line == "\n" || line == "\r\n" {
				return buf, nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(buf) > 0 {
				return buf, nil
			}
			return buf, err
		}
	}
}

func (c *httpWorkerClient) LoadModel(ctx context.Context, worker protocol.WorkerInfo, model string) (bool, error) {
	url := fmt.Sprintf("http://%s:%d/v1/models/load", worker.IP, worker.Port)
	body, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return false, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return false, &WorkerHTTPError{StatusCode: resp.StatusCode, Body: string(raw), WorkerID: worker.ID}
	}
	return true, nil
}

func (c *httpWorkerClient) Capabilities(ctx context.Context, worker protocol.WorkerInfo) (protocol.Capabilities, error) {
	url := fmt.Sprintf("http://%s:%d/capabilities", worker.IP, worker.Port)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return protocol.Capabilities{}, fmt.Errorf("fetching capabilities: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return protocol.Capabilities{}, fmt.Errorf("unexpected status: %s", resp.Status)
	}
	var workerInfo protocol.WorkerInfo
	if err := json.NewDecoder(resp.Body).Decode(&workerInfo); err != nil {
		return protocol.Capabilities{}, fmt.Errorf("decoding capabilities: %w", err)
	}
	return workerInfo.Capabilities, nil
}

func (c *httpWorkerClient) Close() error { return nil }