package protocol

import "testing"

func TestInferenceRequestInterface_ChatRequest(t *testing.T) {
	req := ChatRequest{Model: "m-1", Stream: true}
	var ir InferenceRequest = &req
	if ir.GetModel() != "m-1" {
		t.Fatalf("GetModel = %q, want m-1", ir.GetModel())
	}
	if ir.GetStream() != true {
		t.Fatalf("GetStream = false, want true")
	}
}

func TestInferenceRequestInterface_ChatRequestSetModel(t *testing.T) {
	req := &ChatRequest{Model: "old"}
	var ir InferenceRequest = req
	ir.SetModel("new")
	if req.Model != "new" {
		t.Fatalf("SetModel did not mutate: got %q", req.Model)
	}
}

func TestInferenceRequestInterface_CompletionRequest(t *testing.T) {
	req := CompletionRequest{Model: "c-1", Stream: false}
	var ir InferenceRequest = &req
	if ir.GetModel() != "c-1" {
		t.Fatalf("GetModel = %q, want c-1", ir.GetModel())
	}
	if ir.GetStream() != false {
		t.Fatalf("GetStream = true, want false")
	}
}

func TestInferenceRequestInterface_CompletionRequestSetModel(t *testing.T) {
	req := &CompletionRequest{Model: "old"}
	var ir InferenceRequest = req
	ir.SetModel("new")
	if req.Model != "new" {
		t.Fatalf("SetModel did not mutate: got %q", req.Model)
	}
}
