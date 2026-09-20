package protocol

import (
	"encoding/json"
	"testing"
)

func TestMessageEnvelopeRoundTrip(t *testing.T) {
	raw, err := NewMessage(MsgRegister, RegisterPayload{
		Worker: WorkerInfo{ID: "w1", Hostname: "h", IP: "127.0.0.1", Port: 8081, Transport: TransportWS},
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if raw.ID == "" {
		t.Fatal("expected non-empty message ID")
	}
	if raw.Type != MsgRegister {
		t.Fatalf("type = %q, want %q", raw.Type, MsgRegister)
	}

	// Marshal the envelope to wire bytes and decode it back.
	wire, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var got Message
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if got.ID != raw.ID || got.Type != raw.Type {
		t.Fatalf("envelope mismatch: got %+v want %+v", got, raw)
	}
	var payload RegisterPayload
	if err := got.DecodePayload(&payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Worker.ID != "w1" || payload.Worker.Transport != TransportWS {
		t.Fatalf("payload mismatch: %+v", payload)
	}
}

func TestMessageNilPayloadRoundTrip(t *testing.T) {
	msg, err := NewMessage(MsgPing, nil)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	wire, _ := json.Marshal(msg)
	var got Message
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var p PingPayload
	if err := got.DecodePayload(&p); err != nil {
		t.Fatalf("decode empty payload: %v", err)
	}
}

// TestInferencePayloadsRoundTrip covers every message payload type used by
// the streaming protocol: request -> N chunks -> done response.
func TestInferencePayloadsRoundTrip(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"model":    "llama-3-8b",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})

	req := InferenceRequestPayload{Kind: "chat", Model: "llama-3-8b", Body: body}
	reqMsg, err := NewMessage(MsgInferenceRequest, req)
	if err != nil {
		t.Fatalf("NewMessage request: %v", err)
	}
	var reqGot Message
	_ = json.Unmarshal(mustMarshal(t, reqMsg), &reqGot)
	var reqPayload InferenceRequestPayload
	if err := reqGot.DecodePayload(&reqPayload); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if reqPayload.Kind != "chat" || reqPayload.Model != "llama-3-8b" || len(reqPayload.Body) == 0 {
		t.Fatalf("request payload mismatch: %+v", reqPayload)
	}

	chunk := InferenceChunkPayload{Kind: "chat", Chunk: json.RawMessage(`{"id":"c","choices":[]}`)}
	chunkMsg, err := NewMessage(MsgInferenceChunk, chunk)
	if err != nil {
		t.Fatalf("NewMessage chunk: %v", err)
	}
	var chunkGot Message
	_ = json.Unmarshal(mustMarshal(t, chunkMsg), &chunkGot)
	var chunkPayload InferenceChunkPayload
	if err := chunkGot.DecodePayload(&chunkPayload); err != nil {
		t.Fatalf("decode chunk: %v", err)
	}
	if string(chunkPayload.Chunk) != `{"id":"c","choices":[]}` {
		t.Fatalf("chunk payload mismatch: %s", chunkPayload.Chunk)
	}

	done := InferenceResponsePayload{Kind: "chat", Done: true, Response: json.RawMessage(`{"id":"r"}`)}
	doneMsg, err := NewMessage(MsgInferenceResponse, done)
	if err != nil {
		t.Fatalf("NewMessage done: %v", err)
	}
	var doneGot Message
	_ = json.Unmarshal(mustMarshal(t, doneMsg), &doneGot)
	var donePayload InferenceResponsePayload
	if err := doneGot.DecodePayload(&donePayload); err != nil {
		t.Fatalf("decode done: %v", err)
	}
	if !donePayload.Done || len(donePayload.Response) == 0 {
		t.Fatalf("done payload mismatch: %+v", donePayload)
	}

	errResp := InferenceResponsePayload{Kind: "chat", Done: true, Error: "backend exploded"}
	errMsg, err := NewMessage(MsgInferenceResponse, errResp)
	if err != nil {
		t.Fatalf("NewMessage error response: %v", err)
	}
	var errGot Message
	_ = json.Unmarshal(mustMarshal(t, errMsg), &errGot)
	var errPayload InferenceResponsePayload
	if err := errGot.DecodePayload(&errPayload); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errPayload.Error != "backend exploded" {
		t.Fatalf("error payload mismatch: %+v", errPayload)
	}
}

func TestModelLoadPayloadsRoundTrip(t *testing.T) {
	req := ModelLoadRequestPayload{Model: "llama-3-8b"}
	reqMsg, err := NewMessage(MsgModelLoadRequest, req)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	var got Message
	_ = json.Unmarshal(mustMarshal(t, reqMsg), &got)
	var p ModelLoadRequestPayload
	if err := got.DecodePayload(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Model != "llama-3-8b" {
		t.Fatalf("model load request payload mismatch: %+v", p)
	}

	resp := ModelLoadResponsePayload{Model: "llama-3-8b", Loaded: true}
	respMsg, err := NewMessage(MsgModelLoadResponse, resp)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	var gotResp Message
	_ = json.Unmarshal(mustMarshal(t, respMsg), &gotResp)
	var rp ModelLoadResponsePayload
	if err := gotResp.DecodePayload(&rp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !rp.Loaded || rp.Model != "llama-3-8b" {
		t.Fatalf("model load response payload mismatch: %+v", rp)
	}
}

func TestNewMessageID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewMessageID()
		if id == "" {
			t.Fatal("empty message ID")
		}
		if seen[id] {
			t.Fatalf("duplicate message ID %q", id)
		}
		seen[id] = true
	}
}

func mustMarshal(t *testing.T, v interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}