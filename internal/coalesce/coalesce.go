// Package coalesce batches small writes into fewer, larger messages.
//
// Every PTY read (session) and stdin read (client) currently produces a
// separate QUIC Data message. Fast output like `cat bigfile` generates many
// small packets with per-message overhead (13-byte header each). The Coalescer
// accumulates bytes and flushes when:
//
//   - 2ms deadline expires (measured from first byte in batch, NOT reset by
//     subsequent adds — deadline semantics, not debounce)
//   - 32KB threshold exceeded (matches PTY/stdin read buffer size)
//   - Explicit Flush() at reconnection/shutdown boundaries
package coalesce

import "time"

const (
	// Delay is the coalescing deadline from first byte in batch.
	Delay = 2 * time.Millisecond

	// Threshold triggers an immediate flush when exceeded.
	Threshold = 32 * 1024 // 32 KB, matches PTY/stdin read buffer
)

// Coalescer accumulates bytes and flushes on deadline or threshold.
// All methods are used from a single goroutine (the select loop).
type Coalescer struct {
	buf   []byte
	timer *time.Timer
	armed bool // true when timer is running
}

// New creates a Coalescer with default settings.
func New() *Coalescer {
	t := time.NewTimer(0)
	// Drain the initial fire from NewTimer(0) so Timer() starts clean
	if !t.Stop() {
		<-t.C
	}
	return &Coalescer{
		buf:   make([]byte, 0, Threshold+4096),
		timer: t,
	}
}

// Add appends data to the buffer. Returns true if the threshold was hit
// and the caller should flush immediately.
//
// Arms the deadline timer on the first byte in a batch (when buffer was empty).
// Subsequent adds do NOT reset the timer — this is deadline semantics, not debounce.
func (c *Coalescer) Add(data []byte) bool {
	if len(data) == 0 {
		return false
	}

	// Arm timer on first byte in batch
	if len(c.buf) == 0 && !c.armed {
		c.timer.Reset(Delay)
		c.armed = true
	}

	c.buf = append(c.buf, data...)
	return len(c.buf) >= Threshold
}

// Flush returns the accumulated data and resets the buffer.
// Returns nil if the buffer is empty. The returned slice is a copy
// that the caller owns.
func (c *Coalescer) Flush() []byte {
	if len(c.buf) == 0 {
		return nil
	}

	// Stop timer (don't leak)
	if c.armed {
		if !c.timer.Stop() {
			// Timer already fired — drain the channel so it doesn't
			// trigger a spurious select case later.
			select {
			case <-c.timer.C:
			default:
			}
		}
		c.armed = false
	}

	// Return a copy so caller owns the data
	out := make([]byte, len(c.buf))
	copy(out, c.buf)
	c.buf = c.buf[:0]
	return out
}

// Timer returns the channel that fires when the coalescing deadline expires.
// Use this in a select statement:
//
//	case <-coal.Timer():
//	    data := coal.Flush()
//	    // send data
//
// Returns a nil channel when no deadline is active (nil channels block forever
// in select, effectively disabling the case).
func (c *Coalescer) Timer() <-chan time.Time {
	if !c.armed {
		return nil
	}
	return c.timer.C
}

// Stop releases the timer. Call in defer when done with the Coalescer.
func (c *Coalescer) Stop() {
	c.timer.Stop()
	c.armed = false
}

// Pending returns the number of buffered bytes.
func (c *Coalescer) Pending() int {
	return len(c.buf)
}
