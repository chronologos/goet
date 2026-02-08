package catchup

import "sync"

const DefaultBufferSize = 64 * 1024 * 1024 // 64 MB

// entry stores a single Data message's payload with its sequence number.
type entry struct {
	seq     uint64
	payload []byte
}

// Buffer is a ring buffer that stores terminal output for reconnection catchup.
// It is indexed by monotonic sequence numbers. When the buffer is full, the
// oldest entries are evicted.
//
// Buffer is safe for concurrent use.
type Buffer struct {
	mu       sync.Mutex
	entries  []entry
	size     int // current total payload bytes stored
	maxSize  int // maximum total payload bytes
	head     int // index of next write position
	count    int // number of entries stored
	capacity int // max number of entries (slots in ring)
}

// New creates a catchup buffer with the given maximum byte size.
// The ring has a fixed number of entry slots; the byte limit is soft
// (we evict when totalBytes exceeds maxSize after a store).
func New(maxBytes int) *Buffer {
	if maxBytes <= 0 {
		maxBytes = DefaultBufferSize
	}
	// Estimate slot count: assume average message ~1KB, clamped to [256, 1M].
	slots := max(256, min(maxBytes/1024, 1<<20))
	return &Buffer{
		entries:  make([]entry, slots),
		maxSize:  maxBytes,
		capacity: slots,
	}
}

// Store adds a data payload with the given sequence number.
// If the buffer is full (by bytes), the oldest entries are evicted.
func (b *Buffer) Store(seq uint64, payload []byte) {
	// Copy payload — caller may reuse the slice
	p := make([]byte, len(payload))
	copy(p, payload)

	b.mu.Lock()
	defer b.mu.Unlock()

	// Evict if we'd exceed byte limit
	for b.count > 0 && b.size+len(p) > b.maxSize {
		b.evictOldest()
	}

	// Evict if ring is full by slot count
	if b.count >= b.capacity {
		b.evictOldest()
	}

	b.entries[b.head] = entry{seq: seq, payload: p}
	b.head = (b.head + 1) % b.capacity
	b.count++
	b.size += len(p)
}

// tail returns the index of the oldest entry. Caller must hold b.mu and ensure b.count > 0.
func (b *Buffer) tail() int {
	return (b.head - b.count + b.capacity) % b.capacity
}

// newest returns the index of the most recent entry. Caller must hold b.mu and ensure b.count > 0.
func (b *Buffer) newest() int {
	return (b.head - 1 + b.capacity) % b.capacity
}

func (b *Buffer) evictOldest() {
	t := b.tail()
	b.size -= len(b.entries[t].payload)
	b.entries[t] = entry{} // release memory
	b.count--
}

// ReplaySince returns all stored entries with sequence number > afterSeq,
// in order. Returns nil if no entries qualify.
func (b *Buffer) ReplaySince(afterSeq uint64) []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.count == 0 {
		return nil
	}

	var result []Entry
	t := b.tail()

	for i := 0; i < b.count; i++ {
		idx := (t + i) % b.capacity
		e := b.entries[idx]
		if e.seq > afterSeq {
			result = append(result, Entry{Seq: e.seq, Payload: e.payload})
		}
	}

	return result
}

// Entry is a public view of a stored catchup entry.
type Entry struct {
	Seq     uint64
	Payload []byte
}

// OldestSeq returns the oldest sequence number in the buffer, or 0 if empty.
func (b *Buffer) OldestSeq() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.count == 0 {
		return 0
	}
	return b.entries[b.tail()].seq
}

// NewestSeq returns the newest sequence number in the buffer, or 0 if empty.
func (b *Buffer) NewestSeq() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.count == 0 {
		return 0
	}
	return b.entries[b.newest()].seq
}

