package worker

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CircuitState represents the state of the circuit breaker.
type CircuitState int

const (
	CircuitClosed   CircuitState = iota // Normal operation
	CircuitHalfOpen                     // Testing if backend recovered
	CircuitOpen                         // Backend is down, fail fast
)

func (cs CircuitState) String() string {
	return [...]string{"closed", "half-open", "open"}[cs]
}

// CircuitBreaker implements the circuit breaker pattern for resilience.
type CircuitBreaker struct {
	mu               sync.RWMutex
	state            CircuitState
	failureCount     int
	failureThreshold int
	successThreshold int
	lastFailureTime  time.Time
	cooldownPeriod   time.Duration
}

// NewCircuitBreaker creates a new circuit breaker.
func NewCircuitBreaker(failureThreshold int, cooldownPeriod time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:            CircuitClosed,
		failureThreshold: failureThreshold,
		successThreshold: 1,
		cooldownPeriod:   cooldownPeriod,
	}
}

// RecordSuccess records a successful operation.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.state == CircuitHalfOpen {
		cb.state = CircuitClosed
		cb.failureCount = 0
	}
}

// RecordFailure records a failed operation.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failureCount++
	cb.lastFailureTime = time.Now()
	if cb.failureCount >= cb.failureThreshold {
		cb.state = CircuitOpen
	}
}

// CanExecute checks if an operation can be executed.
//
// State machine:
//   - CircuitClosed  → true (normal operation).
//   - CircuitHalfOpen → true (one probe request allowed).
//   - CircuitOpen    → false while cooldown has not elapsed.
//     After cooldown elapses, transitions to CircuitHalfOpen and returns true
//     (the probe request). The caller must call RecordSuccess or RecordFailure
//     to resolve the half-open state.
func (cb *CircuitBreaker) CanExecute() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case CircuitClosed:
		return true
	case CircuitHalfOpen:
		return true // Allow one request through
	case CircuitOpen:
		// Check if cooldown period has passed
		if time.Since(cb.lastFailureTime) > cb.cooldownPeriod {
			// Transition to half-open: allow one probe request through.
			cb.state = CircuitHalfOpen
			return true
		}
		return false
	}
	return true
}

// GetState returns the current circuit breaker state.
func (cb *CircuitBreaker) GetState() CircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// retryWithBackoff implements exponential backoff retry logic.
func retryWithBackoff(ctx context.Context, maxRetries int, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if err := fn(); err != nil {
			lastErr = err
			backoff := time.Duration(1<<attempt) * time.Second
			select {
			case <-time.After(backoff):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	return fmt.Errorf("max retries (%d) exceeded: %w", maxRetries, lastErr)
}
