package coalesce

import (
	"testing"
	"time"
)

func TestAddAndFlush(t *testing.T) {
	c := New()
	defer c.Stop()

	c.Add([]byte("hello"))
	if len(c.buf) != 5 {
		t.Fatalf("expected 5 pending, got %d", len(c.buf))
	}

	data := c.Flush()
	if string(data) != "hello" {
		t.Fatalf("expected 'hello', got %q", data)
	}

	// After flush, empty
	if len(c.buf) != 0 {
		t.Fatalf("expected 0 pending after flush, got %d", len(c.buf))
	}
	if c.Flush() != nil {
		t.Fatal("expected nil from second flush")
	}
}

func TestThreshold(t *testing.T) {
	c := New()
	defer c.Stop()

	chunk := make([]byte, 1024)
	for range Threshold/1024 - 1 {
		if c.Add(chunk) {
			t.Fatal("should not hit threshold yet")
		}
	}

	if !c.Add(chunk) {
		t.Fatal("should hit threshold")
	}
}

func TestTimerFires(t *testing.T) {
	c := New()
	defer c.Stop()

	c.Add([]byte("x"))

	timer := c.Timer()
	if timer == nil {
		t.Fatal("timer should be non-nil after Add")
	}

	select {
	case <-timer:
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timer should have fired within 100ms")
	}
}

func TestTimerNotResetOnSubsequentAdd(t *testing.T) {
	c := New()
	defer c.Stop()

	c.Add([]byte("first"))
	t1 := time.Now()

	time.Sleep(1 * time.Millisecond) // 1ms into the 2ms deadline
	c.Add([]byte("second"))

	// Timer should fire around 2ms from first add, not from second
	select {
	case <-c.Timer():
		elapsed := time.Since(t1)
		if elapsed > 10*time.Millisecond {
			t.Fatalf("timer took too long: %v (deadline not reset)", elapsed)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timer should have fired")
	}
}

func TestFlushStopsTimer(t *testing.T) {
	c := New()
	defer c.Stop()

	c.Add([]byte("data"))
	c.Flush()

	// Timer channel should now be nil (no deadline active)
	if c.Timer() != nil {
		t.Fatal("timer should be nil after flush")
	}
}

func TestFlushReturnsCopy(t *testing.T) {
	c := New()
	defer c.Stop()

	c.Add([]byte("first"))
	data1 := c.Flush()

	c.Add([]byte("second"))
	data2 := c.Flush()

	// data1 should still be "first", not corrupted by second batch
	if string(data1) != "first" {
		t.Fatalf("first flush corrupted: got %q", data1)
	}
	if string(data2) != "second" {
		t.Fatalf("second flush wrong: got %q", data2)
	}
}

func TestEmptyFlush(t *testing.T) {
	c := New()
	defer c.Stop()

	if c.Flush() != nil {
		t.Fatal("expected nil from empty flush")
	}
}

func TestEmptyAdd(t *testing.T) {
	c := New()
	defer c.Stop()

	if c.Add(nil) {
		t.Fatal("nil add should return false")
	}
	if c.Add([]byte{}) {
		t.Fatal("empty add should return false")
	}
	if len(c.buf) != 0 {
		t.Fatal("pending should be 0 after empty adds")
	}
}

func TestAccumulates(t *testing.T) {
	c := New()
	defer c.Stop()

	c.Add([]byte("hello "))
	c.Add([]byte("world"))

	data := c.Flush()
	if string(data) != "hello world" {
		t.Fatalf("expected 'hello world', got %q", data)
	}
}

func TestTimerNilWhenEmpty(t *testing.T) {
	c := New()
	defer c.Stop()

	if c.Timer() != nil {
		t.Fatal("timer should be nil when no data buffered")
	}
}

// --- Fuzz tests ---

// FuzzCoalescerDataIntegrity adds random chunks in random-sized batches,
// flushing periodically, and verifies the concatenation of all flushed data
// equals the concatenation of all added data. This catches any data loss,
// corruption, or reordering in the Add/Flush cycle.
func FuzzCoalescerDataIntegrity(f *testing.F) {
	f.Add([]byte("hello world"), 3, 5)
	f.Add([]byte{}, 1, 1)
	f.Add([]byte("abcdefghij"), 2, 4)
	f.Fuzz(func(t *testing.T, data []byte, nChunks int, flushEvery int) {
		if nChunks < 0 {
			nChunks = -nChunks
		}
		nChunks = nChunks%20 + 1 // 1..20 chunks
		if flushEvery < 0 {
			flushEvery = -flushEvery
		}
		flushEvery = flushEvery%5 + 1 // flush every 1..5 adds

		c := New()
		defer c.Stop()

		var allInput []byte
		var allOutput []byte

		// Split data into nChunks roughly-equal pieces and add them
		for i := 0; i < nChunks; i++ {
			start := len(data) * i / nChunks
			end := len(data) * (i + 1) / nChunks
			chunk := data[start:end]

			allInput = append(allInput, chunk...)
			c.Add(chunk)

			if (i+1)%flushEvery == 0 {
				if flushed := c.Flush(); flushed != nil {
					allOutput = append(allOutput, flushed...)
				}
			}
		}

		// Final flush
		if flushed := c.Flush(); flushed != nil {
			allOutput = append(allOutput, flushed...)
		}

		// Core invariant: no data lost, no data corrupted
		if len(allInput) != len(allOutput) {
			t.Fatalf("length mismatch: input %d bytes, output %d bytes", len(allInput), len(allOutput))
		}
		for i := range allInput {
			if allInput[i] != allOutput[i] {
				t.Fatalf("byte mismatch at offset %d: input 0x%02x, output 0x%02x", i, allInput[i], allOutput[i])
			}
		}
	})
}
