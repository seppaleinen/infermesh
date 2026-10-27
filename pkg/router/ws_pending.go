package router

import (
	"errors"

	"github.com/seppaleinen/infermesh/pkg/protocol"
)

// streamPendingHandler returns a message handler for streaming inference calls.
// It decodes MsgInferenceChunk and MsgInferenceResponse, frames them as SSE
// data lines, and sends them on pc.chunkCh. MsgError terminates the stream
// with an error. The returned closure captures pc and may be assigned to
// pc.handle.
func streamPendingHandler(pc *pendingCall) func(protocol.Message) {
	return func(msg protocol.Message) {
		switch msg.Type {
		case protocol.MsgInferenceChunk:
			var p protocol.InferenceChunkPayload
			if err := msg.DecodePayload(&p); err != nil {
				pc.fail(errors.New("bad chunk payload: " + err.Error()))
				return
			}
			framed := append([]byte("data: "), p.Chunk...)
			framed = append(framed, '\n', '\n')
			select {
			case pc.chunkCh <- StreamEvent{Data: framed}:
			case <-pc.doneCh:
			}
		case protocol.MsgInferenceResponse:
			var p protocol.InferenceResponsePayload
			if err := msg.DecodePayload(&p); err != nil {
				pc.fail(errors.New("bad response payload: " + err.Error()))
				return
			}
			if p.Error != "" {
				pc.fail(errors.New(p.Error))
				return
			}
			if len(p.Response) > 0 {
				framed := append([]byte("data: "), p.Response...)
				framed = append(framed, '\n', '\n')
				select {
				case pc.chunkCh <- StreamEvent{Data: framed}:
				case <-pc.doneCh:
				}
			}
			if p.Done {
				pc.completeStream()
			}
		case protocol.MsgError:
			var p protocol.ErrorPayload
			_ = msg.DecodePayload(&p)
			pc.fail(errors.New(p.Message))
		}
	}
}

// callPendingHandler returns a message handler for single-response calls
// (non-streaming inference, model preload). It forwards MsgInferenceResponse
// and MsgModelLoadResponse to pc.respCh and calls pc.completeCall().
// MsgError terminates the call with an error. The returned closure captures
// pc and may be assigned to pc.handle.
func callPendingHandler(pc *pendingCall) func(protocol.Message) {
	return func(msg protocol.Message) {
		switch msg.Type {
		case protocol.MsgInferenceResponse:
			select {
			case pc.respCh <- msg:
			case <-pc.doneCh:
			}
			pc.completeCall()
		case protocol.MsgModelLoadResponse:
			select {
			case pc.respCh <- msg:
			case <-pc.doneCh:
			}
			pc.completeCall()
		case protocol.MsgError:
			var p protocol.ErrorPayload
			_ = msg.DecodePayload(&p)
			pc.fail(errors.New(p.Message))
		}
	}
}