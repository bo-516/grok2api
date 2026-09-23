package server

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// errBusy means the queue wait exceeded -queue-timeout.
// The handler turns it into 429 agent_mock_busy with Retry-After: 5.
var errBusy = errors.New("agent-mock is at its grok concurrency limit. Retry after a few seconds.")

// limiter is a semaphore with a bounded queue wait.
// Inflight counts runs that hold a slot, not callers still waiting.
type limiter struct {
	// slots is the counting semaphore. A send acquires, a receive releases.
	slots chan struct{}
	// wait is the maximum queue time.
	wait time.Duration
	// inflight is the number of acquired slots.
	inflight atomic.Int64
}

// newLimiter builds a limiter. n < 1 is treated as 1 so a misconfig cannot deadlock at 0.
func newLimiter(n int, wait time.Duration) *limiter {
	if n < 1 {
		n = 1
	}
	return &limiter{slots: make(chan struct{}, n), wait: wait}
}

// Acquire waits for a slot. It returns errBusy when wait elapses, or ctx.Err()
// when the client goes away. A successful Acquire must be matched with Release.
func (l *limiter) Acquire(ctx context.Context) error {
	timer := time.NewTimer(l.wait)
	defer timer.Stop()
	select {
	case l.slots <- struct{}{}:
		l.inflight.Add(1)
		return nil
	case <-timer.C:
		return errBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release returns a slot. Calling it without Acquire panics on the receive
// only if the buffer is empty, which means the handler double-released.
func (l *limiter) Release() {
	<-l.slots
	l.inflight.Add(-1)
}

// Inflight is the number of grok runs holding a slot.
func (l *limiter) Inflight() int {
	return int(l.inflight.Load())
}
