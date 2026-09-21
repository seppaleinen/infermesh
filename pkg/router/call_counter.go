package router

import (
	"context"
	"sync"
	"time"
)

// callEvent records a single model call with its timestamp.
type callEvent struct {
	model string
	when  time.Time
}

// CallCounter tracks model call counts over a rolling window.
type CallCounter struct {
	mu      sync.Mutex
	counts  map[string]int64 // model -> count in current window
	window  time.Duration
	events  []callEvent // sorted by time, oldest first
	pruneCh chan struct{}
}

// NewCallCounter creates a new CallCounter with the specified rolling window duration.
func NewCallCounter(window time.Duration) *CallCounter {
	return &CallCounter{
		counts:  make(map[string]int64),
		window:  window,
		events:  make([]callEvent, 0),
		pruneCh: make(chan struct{}),
	}
}

// Record records a call for the given model.
func (c *CallCounter) Record(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	c.events = append(c.events, callEvent{model: model, when: now})

	// Increment count for this model
	c.counts[model]++

	// Prune expired events from the end of the list/map
	c.prune(now)
}

// Snapshot returns a snapshot of current counts, pruning expired events first.
func (c *CallCounter) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.prune(time.Now())

	// Return a copy to avoid race conditions on map access
	snapshot := make(map[string]int64, len(c.counts))
	for model, count := range c.counts {
		snapshot[model] = count
	}
	return snapshot
}

// Start starts the periodic pruning goroutine.
func (c *CallCounter) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(c.window / 2)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.mu.Lock()
				c.prune(time.Now())
				c.mu.Unlock()
			case <-ctx.Done():
				return
			case <-c.pruneCh:
				c.mu.Lock()
				c.prune(time.Now())
				c.mu.Unlock()
			}
		}
	}()
}

// Stop stops the pruning goroutine.
func (c *CallCounter) Stop() {
	close(c.pruneCh)
}

// prune removes events older than the rolling window.
func (c *CallCounter) prune(now time.Time) {
	threshold := now.Add(-c.window)
	i := 0
	for i < len(c.events) && c.events[i].when.Before(threshold) {
		// Decrement count for the model being pruned
		if count, ok := c.counts[c.events[i].model]; ok {
			if count <= 1 {
				delete(c.counts, c.events[i].model)
			} else {
				c.counts[c.events[i].model] = count - 1
			}
		}
		i++
	}

	if i > 0 {
		c.events = c.events[i:]
	}
}
