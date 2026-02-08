package catchup

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

func TestStoreAndReplay(t *testing.T) {
	buf := New(1024)

	buf.Store(1, []byte("hello"))
	buf.Store(2, []byte("world"))
	buf.Store(3, []byte("!"))

	entries := buf.ReplaySince(0)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if string(entries[0].Payload) != "hello" {
		t.Fatalf("expected 'hello', got %q", entries[0].Payload)
	}
	if entries[2].Seq != 3 {
		t.Fatalf("expected seq 3, got %d", entries[2].Seq)
	}
}

func TestReplaySinceMiddle(t *testing.T) {
	buf := New(1024)

	buf.Store(1, []byte("a"))
	buf.Store(2, []byte("b"))
	buf.Store(3, []byte("c"))
	buf.Store(4, []byte("d"))

	entries := buf.ReplaySince(2)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Seq != 3 || entries[1].Seq != 4 {
		t.Fatalf("wrong sequences: %d, %d", entries[0].Seq, entries[1].Seq)
	}
}

func TestReplayFullyCaughtUp(t *testing.T) {
	buf := New(1024)
	buf.Store(1, []byte("a"))

	entries := buf.ReplaySince(1)
	if entries != nil {
		t.Fatalf("expected nil, got %d entries", len(entries))
	}
}

func TestReplayEmptyBuffer(t *testing.T) {
	buf := New(1024)
	entries := buf.ReplaySince(0)
	if entries != nil {
		t.Fatalf("expected nil, got %d entries", len(entries))
	}
}

func TestByteEviction(t *testing.T) {
	// Buffer holds max 100 bytes of payload
	buf := New(100)

	// Store 10 entries of 20 bytes each = 200 bytes total
	// Buffer should evict to stay under 100
	for i := uint64(1); i <= 10; i++ {
		buf.Store(i, bytes.Repeat([]byte("x"), 20))
	}

	if buf.Size() > 100 {
		t.Fatalf("buffer size %d exceeds max 100", buf.Size())
	}

	// Newest entries should survive
	newest := buf.NewestSeq()
	if newest != 10 {
		t.Fatalf("expected newest seq 10, got %d", newest)
	}

	// Oldest should have been evicted
	oldest := buf.OldestSeq()
	if oldest <= 1 {
		t.Fatalf("expected eviction of early entries, oldest is %d", oldest)
	}
}

func TestSlotEviction(t *testing.T) {
	// Very large byte limit but small slot count due to small maxBytes input
	buf := New(256) // 256 bytes → 256 slots

	// Store more than slot count
	for i := uint64(1); i <= 300; i++ {
		buf.Store(i, []byte("x"))
	}

	if buf.Count() > buf.capacity {
		t.Fatalf("count %d exceeds capacity %d", buf.Count(), buf.capacity)
	}
	if buf.NewestSeq() != 300 {
		t.Fatalf("expected newest 300, got %d", buf.NewestSeq())
	}
}

func TestCanCatchupFull(t *testing.T) {
	buf := New(1024)
	buf.Store(1, []byte("a"))
	buf.Store(2, []byte("b"))
	buf.Store(3, []byte("c"))

	full, partial := buf.CanCatchup(1)
	if !full {
		t.Fatal("should be full catchup")
	}
	if partial {
		t.Fatal("should not be partial")
	}
}

func TestCanCatchupPartial(t *testing.T) {
	buf := New(50)
	// Fill and evict
	for i := uint64(1); i <= 20; i++ {
		buf.Store(i, bytes.Repeat([]byte("x"), 10))
	}

	// Ask for seq 1, which has been evicted
	full, partial := buf.CanCatchup(1)
	if full {
		t.Fatal("should not be full catchup — old entries evicted")
	}
	if !partial {
		t.Fatal("should be partial catchup")
	}
}

func TestCanCatchupCaughtUp(t *testing.T) {
	buf := New(1024)
	buf.Store(1, []byte("a"))

	full, partial := buf.CanCatchup(1)
	if full || partial {
		t.Fatal("client is caught up, should return false/false")
	}
}

func TestCanCatchupEmpty(t *testing.T) {
	buf := New(1024)
	full, partial := buf.CanCatchup(0)
	if full || partial {
		t.Fatal("empty buffer should return false/false")
	}
}

func TestOldestNewestSeq(t *testing.T) {
	buf := New(1024)
	if buf.OldestSeq() != 0 || buf.NewestSeq() != 0 {
		t.Fatal("empty buffer should return 0")
	}

	buf.Store(5, []byte("a"))
	buf.Store(10, []byte("b"))

	if buf.OldestSeq() != 5 {
		t.Fatalf("expected oldest 5, got %d", buf.OldestSeq())
	}
	if buf.NewestSeq() != 10 {
		t.Fatalf("expected newest 10, got %d", buf.NewestSeq())
	}
}

func TestPayloadCopied(t *testing.T) {
	buf := New(1024)

	data := []byte("original")
	buf.Store(1, data)

	// Mutate the original slice
	data[0] = 'X'

	entries := buf.ReplaySince(0)
	if string(entries[0].Payload) != "original" {
		t.Fatalf("buffer should copy payload, got %q", entries[0].Payload)
	}
}

func TestConcurrentAccess(t *testing.T) {
	buf := New(10240)
	var wg sync.WaitGroup

	// Concurrent writers
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(base uint64) {
			defer wg.Done()
			for i := uint64(0); i < 100; i++ {
				buf.Store(base+i, []byte(fmt.Sprintf("data-%d", base+i)))
			}
		}(uint64(g) * 100)
	}

	// Concurrent readers
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				buf.ReplaySince(0)
				buf.CanCatchup(0)
				buf.OldestSeq()
				buf.NewestSeq()
			}
		}()
	}

	wg.Wait()

	// Just verify no panic/race — exact count depends on scheduling
	if buf.Count() == 0 {
		t.Fatal("expected some entries after concurrent writes")
	}
}

// --- Fuzz tests ---

// FuzzBufferStoreReplay exercises Store/ReplaySince with arbitrary data,
// checking invariants: size never exceeds max, count never exceeds capacity,
// replay results are ordered.
func FuzzBufferStoreReplay(f *testing.F) {
	f.Add(uint64(1), []byte("hello"), uint64(0))
	f.Add(uint64(100), []byte(""), uint64(50))
	f.Fuzz(func(t *testing.T, seq uint64, payload []byte, replayAfter uint64) {
		buf := New(256)

		// Store several entries to exercise the ring
		buf.Store(seq, payload)
		if seq > 0 {
			buf.Store(seq+1, payload)
		}

		// Size can exceed maxSize when a single payload is larger than maxSize
		// (soft eviction — documented behavior). But with multiple entries,
		// eviction should keep total close to maxSize.
		if buf.Count() > 1 && buf.Size() > 256+len(payload) {
			t.Fatalf("size %d too large with %d entries", buf.Size(), buf.Count())
		}
		if buf.Count() > buf.capacity {
			t.Fatalf("count %d exceeds capacity %d", buf.Count(), buf.capacity)
		}

		entries := buf.ReplaySince(replayAfter)
		for i := 1; i < len(entries); i++ {
			if entries[i].Seq <= entries[i-1].Seq {
				t.Fatalf("replay not ordered: seq %d after %d", entries[i].Seq, entries[i-1].Seq)
			}
		}
	})
}

// FuzzBufferEviction fills the buffer beyond capacity with fuzzed payloads,
// checking that invariants hold after eviction.
func FuzzBufferEviction(f *testing.F) {
	f.Add(10, []byte("payload"))
	f.Fuzz(func(t *testing.T, n int, payload []byte) {
		if n < 0 {
			n = -n
		}
		n = n%500 + 1 // 1..500 entries

		buf := New(128)
		for i := uint64(1); i <= uint64(n); i++ {
			buf.Store(i, payload)
		}

		if buf.Size() < 0 {
			t.Fatalf("negative size %d", buf.Size())
		}
		if buf.Count() < 0 || buf.Count() > buf.capacity {
			t.Fatalf("count %d out of range [0, %d]", buf.Count(), buf.capacity)
		}

		if buf.Count() > 0 {
			if buf.NewestSeq() != uint64(n) {
				t.Fatalf("newest seq = %d, want %d", buf.NewestSeq(), n)
			}
			if buf.OldestSeq() == 0 {
				t.Fatalf("oldest seq is 0 with count %d", buf.Count())
			}
		}
	})
}
