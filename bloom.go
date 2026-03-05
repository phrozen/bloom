// Package bloom provides a space-efficient, concurrent-safe Bloom filter
// with zero allocations per operation.
//
// A Bloom filter is a probabilistic data structure that can answer the question
// "Is this item in the set?" with either a definitive No or a probabilistic
// Probably. There are no false negatives: if Contains returns false, the item
// was never added. False positives are possible but their rate is controlled
// by the filter's size and number of hash functions.
//
// The filter uses [atomic.Uint64] for its bitset, making Add and Contains
// fully safe for concurrent use without any locks. Hashers are recycled via
// [sync.Pool] to eliminate per-call heap allocations.
//
// Hash independence is achieved through the Kirsch-Mitzenmacher optimization:
// a single 64-bit hash is split into two 32-bit halves to simulate k
// independent hash functions. The default hasher is FNV-1a from the standard
// library; a custom [hash.Hash64] can be injected via [WithHashFunc].
//
// Filters implement [encoding.BinaryMarshaler] and [encoding.BinaryUnmarshaler]
// for compact serialization.
package bloom

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
	"math"
	"sync"
	"sync/atomic"
)

// HashFunc is a function that returns a new hash.Hash64.
// By defining an interface for our hasher, we avoid hardcoding a
// specific algorithm. This gives users the flexibility to swap
// the default hasher (FNV-1a) with a faster one (like Murmur3 or xxHash)
// if their use case requires maximum throughput.
type HashFunc func() hash.Hash64

// Filter represents a Bloom filter.
// We use atomic.Uint64 so that Add and Contains are fully concurrent-safe
// without requiring an external mutex. On modern 64-bit architectures,
// atomic operations on aligned 64-bit words are essentially free.
type Filter struct {
	bitset []atomic.Uint64
	m      uint64 // Total number of bits in the filter
	k      uint64 // Number of hash functions to apply per element
	hasher HashFunc
	pool   sync.Pool // Pool of hash.Hash64 instances to avoid per-call allocations
}

// Option configures the Filter using the functional options pattern.
// This pattern allows us to provide sensible defaults while
// remaining open for extension later, without breaking the API.
type Option func(*Filter)

// WithHashFunc allows providing a custom hash function constructor.
func WithHashFunc(h HashFunc) Option {
	return func(f *Filter) {
		f.hasher = h
	}
}

// optimalM calculates the ideal total number of bits (m) needed to store
// `n` elements while maintaining a target false positive rate `p`.
// The formula is: m = - (n * ln(p)) / (ln(2)^2)
func optimalM(n int, p float64) uint64 {
	return uint64(math.Ceil(-1 * float64(n) * math.Log(p) / math.Pow(math.Log(2), 2)))
}

// optimalK calculates the ideal number of hash functions (k) to use
// for a given number of bits (`m`) and expected elements (`n`).
// The formula is: k = (m / n) * ln(2)
func optimalK(m uint64, n int) uint64 {
	return uint64(math.Ceil((float64(m) / float64(n)) * math.Log(2)))
}

// NewFilter creates a Bloom filter directly with a specific bit size (m)
// and number of hash iterations (k).
func NewFilter(m int, k int, opts ...Option) *Filter {
	// Since each uint64 holds 64 bits, we divide `m` by 64.
	// We add 63 before dividing to ensure we round up.
	bits := uint64(m)
	size := (bits + 63) / 64
	f := &Filter{
		bitset: make([]atomic.Uint64, size),
		m:      bits,
		k:      uint64(k),
		hasher: fnv.New64a, // FNV-1a is in the standard library and computationally cheap.
	}

	for _, opt := range opts {
		opt(f)
	}

	// Initialize the sync.Pool with the (possibly overridden) hasher.
	// This eliminates the per-call heap allocation of the hash.Hash64 interface.
	f.pool = sync.Pool{
		New: func() any { return f.hasher() },
	}

	return f
}

// NewFilterFromProbability creates a Bloom filter tailored for an expected
// number of elements (`n`) and a desired false-positive probability (`p`).
// Example: NewFilterFromProbability(10000, 0.01) creates a filter for 10k items
// with a 1% chance of a false positive.
func NewFilterFromProbability(n int, p float64, opts ...Option) *Filter {
	m := optimalM(n, p)
	k := optimalK(m, n)
	return NewFilter(int(m), int(k), opts...)
}

// Add inserts data into the Bloom filter.
// It hashes the data once, splits the 64-bit result into two 32-bit halves,
// and simulates `k` independent hashes via Kirsch-Mitzenmacher.
// This method is safe for concurrent use.
func (f *Filter) Add(data []byte) {
	h := f.pool.Get().(hash.Hash64)
	h.Reset()
	h.Write(data)
	sum := h.Sum64()
	f.pool.Put(h)

	// Split the 64-bit hash into two 32-bit halves for Kirsch-Mitzenmacher
	h1 := sum & 0xffffffff
	h2 := sum >> 32

	for i := range f.k {
		// Simulate k independent hashes: hash_i = h1 + i * h2
		bitIdx := (h1 + i*h2) % f.m

		// Atomically OR the bit to 1, safe for concurrent writes
		f.bitset[bitIdx/64].Or(1 << (bitIdx % 64))
	}
}

// Contains checks if data might be in the set.
// If *any* of the `k` hash positions are 0, the element was definitively never added.
// If *all* of the positions are 1, it *might* have been added (or we have a collision).
// This method is safe for concurrent use.
func (f *Filter) Contains(data []byte) bool {
	h := f.pool.Get().(hash.Hash64)
	h.Reset()
	h.Write(data)
	sum := h.Sum64()
	f.pool.Put(h)

	// Split the 64-bit hash into two 32-bit halves for Kirsch-Mitzenmacher
	h1 := sum & 0xffffffff
	h2 := sum >> 32

	for i := range f.k {
		bitIdx := (h1 + i*h2) % f.m

		// Atomically load the word and check the specific bit
		if (f.bitset[bitIdx/64].Load() & (1 << (bitIdx % 64))) == 0 {
			return false // Definitively not in the set
		}
	}
	return true // Probably in the set
}

// Binary encoding format (little-endian):
//	[0:8]    m       (uint64, total number of bits)
//	[8:16]   k       (uint64, number of hash functions)
//	[16:]    bitset  ((m+63)/64 × 8 bytes, raw uint64 words)

// MarshalBinary implements the encoding.BinaryMarshaler interface.
// It serializes the filter's parameters and bitset into a portable binary format.
// The hash function is NOT serialized — upon UnmarshalBinary, the default
// FNV-1a hasher is used. Use WithHashFunc after unmarshaling if needed.
func (f *Filter) MarshalBinary() ([]byte, error) {
	nwords := uint64(len(f.bitset))
	buf := make([]byte, 8+8+nwords*8) // m + k + bitset

	binary.LittleEndian.PutUint64(buf[0:8], f.m)
	binary.LittleEndian.PutUint64(buf[8:16], f.k)

	for i := range nwords {
		binary.LittleEndian.PutUint64(buf[16+i*8:16+i*8+8], f.bitset[i].Load())
	}

	return buf, nil
}

// UnmarshalBinary implements the encoding.BinaryUnmarshaler interface.
// It restores the filter's parameters and bitset from data produced by
// MarshalBinary. If the receiver was created with a custom hasher (via
// WithHashFunc), that hasher is preserved. Otherwise, it defaults to FNV-1a.
func (f *Filter) UnmarshalBinary(data []byte) error {
	if len(data) < 16 {
		return errors.New("bloom: binary data too short")
	}

	m := binary.LittleEndian.Uint64(data[0:8])
	k := binary.LittleEndian.Uint64(data[8:16])
	nwords := (m + 63) / 64

	expected := 16 + nwords*8
	if uint64(len(data)) != expected {
		return fmt.Errorf("bloom: expected %d bytes, got %d", expected, len(data))
	}

	bitset := make([]atomic.Uint64, nwords)
	for i := range nwords {
		bitset[i].Store(binary.LittleEndian.Uint64(data[16+i*8 : 16+i*8+8]))
	}

	f.m = m
	f.k = k
	f.bitset = bitset
	if f.hasher == nil {
		f.hasher = fnv.New64a
	}
	f.pool = sync.Pool{
		New: func() any { return f.hasher() },
	}

	return nil
}
