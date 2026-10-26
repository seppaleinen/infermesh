package worker

import (
	"context"
	"time"
)

// idleTimeout manages a timer that cancels ctx when no activity occurs for
// the configured duration. A single goroutine owns the timer exclusively,
// so resets are race-free: activity sends a duration on the reset channel,
// and the goroutine stops, drains, and restarts the timer atomically.
//
// The timer starts immediately, so the first-byte wait (model loading) is
// also bounded by the configured duration.
type idleTimeout struct {
	ctx    context.Context
	cancel context.CancelFunc
	timer  *time.Timer
	done   chan struct{}
	// resetCh carries fresh durations from activity callers to the watcher
	// goroutine, which owns the timer exclusively. A single-slot buffer is
	// sufficient: only the most recent reset matters.
	resetCh chan time.Duration
}

func newIdleTimeout(parent context.Context, d time.Duration) *idleTimeout {
	idleCtx, cancel := context.WithCancel(parent)
	it := &idleTimeout{
		ctx:     idleCtx,
		cancel:  cancel,
		timer:   time.NewTimer(d),
		done:    make(chan struct{}),
		resetCh: make(chan time.Duration, 1),
	}
	go it.run()
	return it
}

func (it *idleTimeout) run() {
	defer close(it.done)
	defer it.timer.Stop()
	for {
		select {
		case <-it.timer.C:
			it.cancel()
			return
		case d := <-it.resetCh:
			if !it.timer.Stop() {
				select {
				case <-it.timer.C:
				default:
				}
			}
			it.timer.Reset(d)
		case <-it.ctx.Done():
			return
		}
	}
}

// reset restarts the idle timer with a fresh duration. Non-blocking: if a
// reset is already pending the previous one is dropped (the latest duration
// wins, which is correct — we only care about the most recent activity). If
// the watcher has already exited (idle timeout fired or parent cancelled)
// the reset is a no-op.
func (it *idleTimeout) reset(d time.Duration) {
	select {
	case <-it.done:
		// Watcher exited; nothing to reset.
	case it.resetCh <- d:
	default:
	}
}

// stop cancels the underlying context and waits for the watcher goroutine
// to exit. Safe to call exactly once.
func (it *idleTimeout) stop() {
	it.cancel()
	<-it.done
}
