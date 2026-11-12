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
			pc.deliverChunk(StreamEvent{Data: framed})
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
				pc.deliverChunk(StreamEvent{Data: framed})
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
			delivered, done := pc.deliverResp(msg)
			if !done && !delivered {
				pc.fail(errors.New("response dropped: consumer not reading"))
			}
			pc.completeCall()
		case protocol.MsgModelLoadResponse:
			delivered, done := pc.deliverResp(msg)
			if !done && !delivered {
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

// deliverChunk delivers ev on pc.chunkCh without ever blocking. Returns
// (delivered, done): done means the call is already finished and ev was
// dropped. sendMu serializes the send against finish()'s close(pc.chunkCh):
// a send and a close on the same channel must never interleave (concurrent
// send+close is a data race; sending on a closed channel panics).
func (pc *pendingCall) deliverChunk(ev StreamEvent) (bool, bool) {
	select {
	case <-pc.doneCh:
		return false, true
	default:
	}
	pc.sendMu.Lock()
	select {
	case <-pc.doneCh:
		pc.sendMu.Unlock()
		return false, true
	default:
	}
	select {
	case pc.chunkCh <- ev:
		pc.sendMu.Unlock()
		return true, false
	default:
		pc.sendMu.Unlock()
		return false, false
	}
}

// deliverResp delivers msg on pc.respCh without ever blocking. Returns
// (delivered, done): done means the call is already finished and msg was
// dropped. sendMu serializes the send against finish()'s close(pc.respCh):
// a send and a close on the same channel must never interleave (concurrent
// send+close is a data race; sending on a closed channel panics).
func (pc *pendingCall) deliverResp(msg protocol.Message) (bool, bool) {
	select {
	case <-pc.doneCh:
		return false, true
	default:
	}
	pc.sendMu.Lock()
	select {
	case <-pc.doneCh:
		pc.sendMu.Unlock()
		return false, true
	default:
	}
	select {
	case pc.respCh <- msg:
		pc.sendMu.Unlock()
		return true, false
	default:
		pc.sendMu.Unlock()
		return false, false
	}
}
