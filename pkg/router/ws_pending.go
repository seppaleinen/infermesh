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
//
// The handler is non-blocking: if the HTTP consumer is not draining pc.chunkCh
// (slow client, disconnected agent), the select falls through to default and
// drops the chunk rather than blocking the reader loop. A blocked reader loop
// is the root cause of the relay↔router wedge: the router stops reading from
// the relay, the relay's single writer goroutine can't drain its send queue,
// worker heartbeats and inference responses are dropped, and the router marks
// the worker unavailable. Dropping a few SSE chunks is far cheaper than
// wedging the entire pool (issue #198).
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
			default:
				// Consumer is full or done; drop the chunk rather than
				// block the reader loop. The HTTP handler will eventually
				// time out or the client will disconnect, closing doneCh.
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
				default:
					// Consumer is full or done; drop the chunk.
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
//
// Like streamPendingHandler, this is non-blocking: if the HTTP consumer is
// not reading pc.respCh, the select falls through to default and fails the
// call rather than blocking the reader loop. Failing the call is correct here
// (the client gets a fast error instead of a hang) whereas silently dropping
// the response would leave the client waiting forever.
func callPendingHandler(pc *pendingCall) func(protocol.Message) {
	return func(msg protocol.Message) {
		switch msg.Type {
		case protocol.MsgInferenceResponse:
			select {
			case pc.respCh <- msg:
			case <-pc.doneCh:
			default:
				// Consumer is full (HTTP handler not reading). Fail the
				// call rather than block the reader loop — the HTTP
				// handler will see the error when it next reads from errCh.
				pc.fail(errors.New("response dropped: consumer not reading"))
			}
			pc.completeCall()
		case protocol.MsgModelLoadResponse:
			select {
			case pc.respCh <- msg:
			case <-pc.doneCh:
			default:
				pc.fail(errors.New("response dropped: consumer not reading"))
			}
			pc.completeCall()
		case protocol.MsgError:
			var p protocol.ErrorPayload
			_ = msg.DecodePayload(&p)
			pc.fail(errors.New(p.Message))
		}
	}
}
