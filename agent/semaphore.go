package agent

import (
	"context"
	"fmt"
)

// querySemaphore bounds the number of DB-touching reverse RPCs the agent
// runs simultaneously. It exists to keep one user's web session from
// spawning hundreds of goroutines on the agent — each waiting for a
// *sqlx.DB connection slot, holding scan buffers, etc. The actual DB
// connection pool already caps simultaneous queries at the SQL driver level
// (SetMaxOpenConns); this semaphore caps the LAYER ABOVE it.
//
// A nil receiver disables the cap entirely — handy for tests and the
// "unlimited" config (0 or negative).
type querySemaphore struct {
	ch       chan struct{}
	capacity int
}

func newQuerySemaphore(capacity int) *querySemaphore {
	if capacity <= 0 {
		return nil
	}
	return &querySemaphore{
		ch:       make(chan struct{}, capacity),
		capacity: capacity,
	}
}

// Acquire blocks until a slot is available or ctx fires. The caller's
// caller is expected to bound ctx (so an "agent overloaded" error gets
// returned instead of the request hanging forever).
func (s *querySemaphore) Acquire(ctx context.Context) error {
	if s == nil {
		return nil
	}
	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("agent overloaded: %d concurrent queries in-flight (cap reached); waited %v", s.capacity, ctx.Err())
	}
}

// Release returns a slot. Safe to call on a nil receiver (matched Acquire).
// Calling more times than Acquire is a programmer error and will block the
// next Acquire indefinitely; pair them with defer.
func (s *querySemaphore) Release() {
	if s == nil {
		return
	}
	<-s.ch
}
