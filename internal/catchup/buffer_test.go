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

	if buf.size > 100 {
		t.Fatalf("buffer size %d exceeds max 100", buf.size)
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

	if buf.count > buf.capacity {
		t.Fatalf("count %d exceeds capacity %d", buf.count, buf.capacity)
	}
	if buf.NewestSeq() != 300 {
		t.Fatalf("expected newest 300, got %d", buf.NewestSeq())
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
				buf.OldestSeq()
				buf.NewestSeq()
			}
		}()
	}

	wg.Wait()

	// Just verify no panic/race — exact count depends on scheduling
	if buf.NewestSeq() == 0 {
		t.Fatal("expected some entries after concurrent writes")
	}
}

// TestPartialCatchupReplayAfterEviction verifies that when entries have been
// evicted from the ring buffer, ReplaySince returns only the surviving entries.
// This simulates a reconnecting client that missed more data than the buffer
// can hold — it gets a correct but incomplete history starting from the oldest
// surviving entry.
func TestPartialCatchupReplayAfterEviction(t *testing.T) {
	// Small buffer: 256 bytes max. With 50-byte payloads, only ~5 entries fit.
	buf := New(256)

	// Store 20 entries of 50 bytes each (1000 bytes total). The first ~15
	// entries must be evicted to stay under the 256-byte limit.
	payloadSize := 50
	totalEntries := uint64(20)
	for i := uint64(1); i <= totalEntries; i++ {
		payload := bytes.Repeat([]byte{byte('A' + i%26)}, payloadSize)
		buf.Store(i, payload)
	}

	// ReplaySince(0) asks for everything — but evicted entries are gone.
	entries := buf.ReplaySince(0)

	if len(entries) == 0 {
		t.Fatal("expected some entries from ReplaySince(0), got none")
	}

	// The oldest surviving entry must have seq > 1 (proving eviction occurred).
	if entries[0].Seq <= 1 {
		t.Fatalf("expected eviction of early entries, but oldest returned seq is %d", entries[0].Seq)
	}

	// The newest entry must be the last one stored.
	if entries[len(entries)-1].Seq != totalEntries {
		t.Fatalf("expected newest seq %d, got %d", totalEntries, entries[len(entries)-1].Seq)
	}

	// Verify all returned entries are contiguous (no gaps in the surviving range).
	for i := 1; i < len(entries); i++ {
		if entries[i].Seq != entries[i-1].Seq+1 {
			t.Fatalf("gap in replay: seq %d followed by %d", entries[i-1].Seq, entries[i].Seq)
		}
	}

	// Verify each payload is correct (matches the pattern from Store).
	for _, e := range entries {
		expected := bytes.Repeat([]byte{byte('A' + e.Seq%26)}, payloadSize)
		if !bytes.Equal(e.Payload, expected) {
			t.Fatalf("payload mismatch at seq %d: got %q, want %q", e.Seq, e.Payload[:5], expected[:5])
		}
	}

	// Verify ReplaySince with a seq that's been evicted still returns
	// only surviving entries (not an error).
	entriesFromEvicted := buf.ReplaySince(2)
	if len(entriesFromEvicted) == 0 {
		t.Fatal("ReplaySince(2) should return surviving entries even though seq 2 was evicted")
	}
	// Should return the same entries as ReplaySince(0) since seq 2 is before
	// the oldest surviving entry anyway.
	if entriesFromEvicted[0].Seq != entries[0].Seq {
		t.Fatalf("ReplaySince(2) oldest seq %d != ReplaySince(0) oldest seq %d",
			entriesFromEvicted[0].Seq, entries[0].Seq)
	}

	t.Logf("buffer holds %d of %d entries (oldest seq=%d, newest seq=%d)",
		len(entries), totalEntries, entries[0].Seq, entries[len(entries)-1].Seq)
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
		if buf.count > 1 && buf.size > 256+len(payload) {
			t.Fatalf("size %d too large with %d entries", buf.size, buf.count)
		}
		if buf.count > buf.capacity {
			t.Fatalf("count %d exceeds capacity %d", buf.count, buf.capacity)
		}

		entries := buf.ReplaySince(replayAfter)
		for i := 1; i < len(entries); i++ {
			if entries[i].Seq <= entries[i-1].Seq {
				t.Fatalf("replay not ordered: seq %d after %d", entries[i].Seq, entries[i-1].Seq)
			}
		}
	})
}

// FuzzBufferDataIntegrity stores entries with unique payloads, then verifies
// that replayed data exactly matches what was stored (for non-evicted entries).
// Catches corruption in the copy-on-store path and ring wraparound logic.
func FuzzBufferDataIntegrity(f *testing.F) {
	f.Add(5, []byte("payload"), 3)
	f.Add(1, []byte{}, 0)
	f.Add(50, []byte("x"), 25)
	f.Fuzz(func(t *testing.T, n int, base []byte, replaySkip int) {
		if n < 0 {
			n = -n
		}
		n = n%100 + 1 // 1..100 entries

		if replaySkip < 0 {
			replaySkip = -replaySkip
		}
		replaySkip = replaySkip % n

		buf := New(4096) // big enough to hold small payloads without eviction

		// Store entries with unique payloads: base + seq suffix
		type stored struct {
			seq     uint64
			payload []byte
		}
		var all []stored
		for i := 0; i < n; i++ {
			seq := uint64(i + 1)
			p := append(base, fmt.Sprintf("-%d", seq)...)
			cp := make([]byte, len(p))
			copy(cp, p)
			buf.Store(seq, p)
			all = append(all, stored{seq: seq, payload: cp})
		}

		// Replay and verify data integrity
		entries := buf.ReplaySince(uint64(replaySkip))
		for _, e := range entries {
			// Find the matching stored entry
			idx := int(e.Seq) - 1
			if idx < 0 || idx >= len(all) {
				t.Fatalf("replayed seq %d out of stored range", e.Seq)
			}
			if !bytes.Equal(e.Payload, all[idx].payload) {
				t.Fatalf("seq %d payload mismatch: got %q, want %q",
					e.Seq, e.Payload, all[idx].payload)
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

		if buf.size < 0 {
			t.Fatalf("negative size %d", buf.size)
		}
		if buf.count < 0 || buf.count > buf.capacity {
			t.Fatalf("count %d out of range [0, %d]", buf.count, buf.capacity)
		}

		if buf.count > 0 {
			if buf.NewestSeq() != uint64(n) {
				t.Fatalf("newest seq = %d, want %d", buf.NewestSeq(), n)
			}
			if buf.OldestSeq() == 0 {
				t.Fatalf("oldest seq is 0 with count %d", buf.count)
			}
		}
	})
}
