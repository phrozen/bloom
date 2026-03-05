package bloom

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"testing"
)

func TestFilter_AddAndContains(t *testing.T) {
	// Create a small filter for testing
	f := NewFilter(100, 3)

	// Test adding and retrieving a single item
	item1 := []byte("apple")
	if f.Contains(item1) {
		t.Errorf("Empty filter should not contain 'apple'")
	}

	f.Add(item1)
	if !f.Contains(item1) {
		t.Errorf("Filter should contain 'apple' after it was added")
	}

	// Test adding multiple items
	items := [][]byte{
		[]byte("banana"),
		[]byte("cherry"),
		[]byte("date"),
	}

	for _, item := range items {
		f.Add(item)
	}

	// Verify all added items are present
	for _, item := range items {
		if !f.Contains(item) {
			t.Errorf("Filter should contain '%s'", item)
		}
	}

	// Verify an unadded item is not present (might fail due to false positive,
	// but highly unlikely with this capacity and item count)
	unadded := []byte("elderberry")
	if f.Contains(unadded) {
		t.Errorf("Filter should not contain '%s' (or we got a very unlucky false positive)", unadded)
	}
}

func TestFilter_EmptyData(t *testing.T) {
	f := NewFilter(100, 3)
	empty := []byte("")

	if f.Contains(empty) {
		t.Errorf("Empty filter should not contain empty string")
	}

	f.Add(empty)

	if !f.Contains(empty) {
		t.Errorf("Filter should contain empty string after it was added")
	}
}

func TestFilter_FalsePositiveRate(t *testing.T) {
	// Let's test the probability math
	// We want to store 10,000 items with a 1% false positive rate
	n := 10000
	p := 0.01

	f := NewFilterFromProbability(n, p)

	// 1. Add 10,000 random items
	addedItems := make([][]byte, n)
	for i := range n {
		b := make([]byte, 16)
		rand.Read(b)
		addedItems[i] = b
		f.Add(b)
	}

	// 2. Verify all added items return true (no false negatives!)
	for _, item := range addedItems {
		if !f.Contains(item) {
			t.Fatalf("False negative detected! Bloom filters MUST never have false negatives.")
		}
	}

	// 3. Check 10,000 items we NEVER added, and count the false positives
	falsePositives := 0
	tests := 10000
	for range tests {
		b := make([]byte, 16)
		rand.Read(b)

		// Very tiny chance we randomly generated a byte slice we already added,
		// but 16 bytes of crypto/rand makes a collision statistically impossible in this test.
		if f.Contains(b) {
			falsePositives++
		}
	}

	// Calculate actual rate
	actualRate := float64(falsePositives) / float64(tests)

	// We expected ~1% (0.01). We should allow some variance for randomness,
	// say up to 1.5% (0.015) before failing the test.
	if actualRate > 0.015 {
		t.Errorf("False positive rate too high! Expected ~%f, got %f (%d/%d)", p, actualRate, falsePositives, tests)
	}

	t.Logf("Configured P: %f, Actual P: %f (m: %d, k: %d)\n", p, actualRate, f.m, f.k)
}

func ExampleNewFilterFromProbability() {
	// Create a filter for 1000 items with a 1% false positive rate
	f := NewFilterFromProbability(1000, 0.01)

	// Add an item
	f.Add([]byte("my-database-key"))

	// Check if it exists
	if f.Contains([]byte("my-database-key")) {
		fmt.Println("Item is probably in the set.")
	}

	if !f.Contains([]byte("non-existent-key")) {
		fmt.Println("Item is definitively NOT in the set.")
	}

	// Output:
	// Item is probably in the set.
	// Item is definitively NOT in the set.
}

// keySize is the size of each key in bytes, matching a UUID (128 bits).
const keySize = 16

// generateRandomBuffer creates a single contiguous random byte slice of
// length n + keySize - 1. By sliding a window of keySize bytes across it,
// we get n unique keys with zero per-key allocations.
// array[0:16] and array[1:17] are entirely different because the underlying
// data is cryptographically random.
func generateRandomBuffer(n int) []byte {
	buf := make([]byte, n+keySize-1)
	rand.Read(buf)
	return buf
}

// key returns the i-th key from a sliding window buffer.
func key(buf []byte, i int) []byte {
	return buf[i : i+keySize]
}

func BenchmarkFilter_Add(b *testing.B) {
	f := NewFilter(1000000, 7)
	buf := generateRandomBuffer(b.N)

	b.ResetTimer()
	for i := range b.N {
		f.Add(key(buf, i))
	}
}

func BenchmarkFilter_Contains(b *testing.B) {
	f := NewFilter(1000000, 7)
	// Pre-fill the filter with 1000 items
	fillBuf := generateRandomBuffer(1000)
	for i := range 1000 {
		f.Add(key(fillBuf, i))
	}

	testBuf := generateRandomBuffer(b.N)

	b.ResetTimer()
	for i := range b.N {
		f.Contains(key(testBuf, i))
	}
}

// --- Micro-benchmarks ---

// BenchmarkFilter_Insert1M measures the throughput of inserting 1M UUID-sized
// keys into a filter sized for 1M items at 1% FPR.
func BenchmarkFilter_Insert1M(b *testing.B) {
	const numItems = 1_000_000
	buf := generateRandomBuffer(numItems)

	b.ResetTimer()
	for range b.N {
		f := NewFilterFromProbability(numItems, 0.01)
		for i := range numItems {
			f.Add(key(buf, i))
		}
	}

	// Report memory footprint of the filter itself
	f := NewFilterFromProbability(numItems, 0.01)
	b.ReportMetric(float64(len(f.bitset)*8), "bytes/filter")
	b.ReportMetric(float64(f.m), "bits")
	b.ReportMetric(float64(f.k), "hash_fns")
}

// BenchmarkFilter_Contains1M populates a filter with 1M UUID-sized keys,
// then benchmarks lookup speed against different random keys.
func BenchmarkFilter_Contains1M(b *testing.B) {
	const numItems = 1_000_000
	f := NewFilterFromProbability(numItems, 0.01)

	// Fill the filter
	fillBuf := generateRandomBuffer(numItems)
	for i := range numItems {
		f.Add(key(fillBuf, i))
	}

	// Generate separate lookup buffer
	lookupBuf := generateRandomBuffer(b.N)

	b.ResetTimer()
	for i := range b.N {
		f.Contains(key(lookupBuf, i))
	}
}

// BenchmarkFilter_ConcurrentAdd measures pure concurrent Add throughput.
// GOMAXPROCS goroutines all write to the same filter simultaneously.
func BenchmarkFilter_ConcurrentAdd(b *testing.B) {
	const numItems = 1_000_000
	f := NewFilterFromProbability(numItems, 0.01)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		buf := generateRandomBuffer(1_000_000)
		i := 0
		for pb.Next() {
			f.Add(key(buf, i%1_000_000))
			i++
		}
	})
}

// BenchmarkFilter_ConcurrentAddContains exercises the atomic.Uint64 design
// under real contention. GOMAXPROCS goroutines do 50/50 Add/Contains on a
// shared filter simultaneously using UUID-sized keys.
func BenchmarkFilter_ConcurrentAddContains(b *testing.B) {
	const numItems = 1_000_000
	f := NewFilterFromProbability(numItems, 0.01)

	// Pre-fill with 1M items so Contains hits are realistic
	fillBuf := generateRandomBuffer(1_000_000)
	for i := range 1_000_000 {
		f.Add(key(fillBuf, i))
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		// Each goroutine gets its own random buffer
		buf := generateRandomBuffer(1_000_000)
		i := 0
		for pb.Next() {
			idx := i % 1_000_000
			if i%2 == 0 {
				f.Add(key(buf, idx))
			} else {
				f.Contains(key(buf, idx))
			}
			i++
		}
	})
}

// --- MarshalBinary / UnmarshalBinary Tests ---

func TestFilter_MarshalUnmarshalBinary(t *testing.T) {
	// Create a filter, add items, marshal, unmarshal, and check membership.
	f := NewFilterFromProbability(10000, 0.01)

	items := [][]byte{
		[]byte("alpha"),
		[]byte("bravo"),
		[]byte("charlie"),
		[]byte("delta"),
	}
	for _, item := range items {
		f.Add(item)
	}

	data, err := f.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary failed: %v", err)
	}

	var restored Filter
	if err := restored.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary failed: %v", err)
	}

	// Parameters must match
	if restored.m != f.m {
		t.Errorf("m mismatch: got %d, want %d", restored.m, f.m)
	}
	if restored.k != f.k {
		t.Errorf("k mismatch: got %d, want %d", restored.k, f.k)
	}
	if len(restored.bitset) != len(f.bitset) {
		t.Fatalf("bitset length mismatch: got %d, want %d", len(restored.bitset), len(f.bitset))
	}

	// All added items must still be found
	for _, item := range items {
		if !restored.Contains(item) {
			t.Errorf("restored filter should contain %q", item)
		}
	}

	// Non-added item should still not be found
	if restored.Contains([]byte("foxtrot")) {
		t.Errorf("restored filter should not contain 'foxtrot' (unlikely false positive)")
	}
}

func TestFilter_MarshalUnmarshalBinary_RoundTrip1M(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 1M round-trip test in short mode")
	}

	const numItems = 1_000_000
	f := NewFilterFromProbability(numItems, 0.01)

	buf := generateRandomBuffer(numItems)
	for i := range numItems {
		f.Add(key(buf, i))
	}

	data, err := f.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary failed: %v", err)
	}

	var restored Filter
	if err := restored.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary failed: %v", err)
	}

	// Verify every inserted key is still present (no false negatives)
	for i := range numItems {
		if !restored.Contains(key(buf, i)) {
			t.Fatalf("false negative at index %d after round-trip", i)
		}
	}
}

func TestFilter_UnmarshalBinary_Errors(t *testing.T) {
	var f Filter

	// Too short
	if err := f.UnmarshalBinary([]byte{1, 2, 3}); err == nil {
		t.Error("expected error for short data")
	}

	// Length mismatch (m=64 means 1 word expected, but no bitset payload)
	short := make([]byte, 16)
	binary.LittleEndian.PutUint64(short[0:8], 64) // m = 64 → 1 word expected
	binary.LittleEndian.PutUint64(short[8:16], 7) // k = 7
	if err := f.UnmarshalBinary(short); err == nil {
		t.Error("expected error for truncated bitset")
	}
}
