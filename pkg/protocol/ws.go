package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// MessageType identifies the kind of a WebSocket control or inference message
// exchanged between a worker and the router on /v1/connect.
type MessageType string

const (
	MsgRegister          MessageType = "register"
	MsgWelcome           MessageType = "welcome"
	MsgHeartbeat         MessageType = "heartbeat"
	MsgCapabilities      MessageType = "capabilities"
	MsgInferenceRequest  MessageType = "inference_request"
	MsgInferenceChunk    MessageType = "inference_chunk"
	MsgInferenceResponse MessageType = "inference_response"
	MsgInferenceCancel   MessageType = "inference_cancel"
	MsgModelLoadRequest  MessageType = "model_load_request"
	MsgModelLoadResponse MessageType = "model_load_response"
	MsgPing              MessageType = "ping"
	MsgError             MessageType = "error"
	MsgClose             MessageType = "close"
	MsgRouterReady       MessageType = "router_ready"
)

// Message is the JSON envelope carried in every WebSocket frame on the
// InferMesh protocol. ID correlates a request with its response(s);
// streaming inference produces N× inference_chunk followed by a single
// inference_response with Done=true.
type Message struct {
	ID      string          `json:"id"`
	Type    MessageType     `json:"type"`
	WorkerID string         `json:"worker_id,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

// NewMessage builds an envelope, marshaling the payload. payload may be nil
// for messages without a body.
func NewMessage(mt MessageType, payload interface{}) (Message, error) {
	var raw json.RawMessage
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return Message{}, fmt.Errorf("marshal %s payload: %w", mt, err)
		}
		raw = data
	}
	return Message{ID: NewMessageID(), Type: mt, Payload: raw}, nil
}

// DecodePayload unmarshals the envelope payload into v.
func (m Message) DecodePayload(v interface{}) error {
	if len(m.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(m.Payload, v)
}

// NewMessageID returns a random hex message identifier used to correlate
// requests and responses. Falls back to a timestamp if the OS entropy source
// is unavailable.
func NewMessageID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// RegisterPayload is sent by a worker immediately after the WebSocket
// handshake completes.
type RegisterPayload struct {
	Worker WorkerInfo `json:"worker"`
	Relay bool        `json:"relay,omitempty"` // true if this is a relay registration
}

// WelcomePayload is sent by the router after it accepts a registration.
type WelcomePayload struct {
	WorkerID   string `json:"worker_id"`
	Superseded bool   `json:"superseded,omitempty"`
	Message    string `json:"message,omitempty"`
}

// HeartbeatPayload refreshes the worker's registration and carries fresh
// capabilities/models so the router never needs to dial back.
type HeartbeatPayload struct {
	Worker WorkerInfo `json:"worker"`
}

// CapabilitiesPayload is a standalone capabilities push, independent of a
// register or heartbeat.
type CapabilitiesPayload struct {
	WorkerID     string       `json:"worker_id"`
	Capabilities Capabilities `json:"capabilities"`
}

// InferenceRequestPayload carries an OpenAI-compatible request body.
// Kind is "chat" or "completion" and selects the worker-side handler.
type InferenceRequestPayload struct {
	Kind  string          `json:"kind"`
	Model string          `json:"model"`
	Body  json.RawMessage `json:"body"`
}

// InferenceChunkPayload is one streamed response chunk (raw JSON bytes).
type InferenceChunkPayload struct {
	Kind  string          `json:"kind"`
	Chunk json.RawMessage `json:"chunk"`
}

// InferenceResponsePayload terminates a streaming sequence (Done=true) or
// carries the single non-streaming response body (Done=true, Response set).
// Error is set when the worker-side backend failed.
type InferenceResponsePayload struct {
	Kind     string          `json:"kind"`
	Done     bool            `json:"done"`
	Error    string          `json:"error,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

// InferenceCancelPayload cancels an in-flight inference by message ID.
type InferenceCancelPayload struct {
	ID string `json:"id"`
}

// ModelLoadRequestPayload asks a worker to load a model.
type ModelLoadRequestPayload struct {
	Model string `json:"model"`
}

// ModelLoadResponsePayload reports the result of a model load attempt.
type ModelLoadResponsePayload struct {
	Model  string `json:"model"`
	Loaded bool   `json:"loaded"`
	Error  string `json:"error,omitempty"`
}

// PingPayload is an idle keepalive. The receiver echoes it back as another
// MsgPing with the same ID.
type PingPayload struct {
	Timestamp int64 `json:"timestamp,omitempty"`
}

// ErrorPayload carries a protocol or application error.
type ErrorPayload struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	ID      string `json:"id,omitempty"`
}

// ClosePayload asks the peer to close the connection (e.g. superseded).
type ClosePayload struct {
	Reason string `json:"reason,omitempty"`
}