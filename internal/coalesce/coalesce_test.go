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
	for i := 0; i < Threshold/1024-1; i++ {
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
