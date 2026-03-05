# bloom

[![Go Reference](https://pkg.go.dev/badge/github.com/phrozen/bloom.svg)](https://pkg.go.dev/github.com/phrozen/bloom)
[![Go Report Card](https://goreportcard.com/badge/github.com/phrozen/bloom)](https://goreportcard.com/report/github.com/phrozen/bloom)
[![Build Status](https://github.com/phrozen/bloom/actions/workflows/go.yml/badge.svg)](https://github.com/phrozen/bloom/actions)
[![LICENSE](https://img.shields.io/github/license/phrozen/bloom)](https://github.com/phrozen/bloom/blob/main/LICENSE)

An ultra fast, lightweight, concurrent-safe [Bloom filter](https://en.wikipedia.org/wiki/Bloom_filter) for Go.

**Zero dependencies** — only the Go standard library. Zero allocations per operation. Lock-free concurrent reads and writes via `atomic.Uint64`. Built for production use, small enough to understand (just over 100 LOC).

> This library is based on my blog post: [The Magic of Bloom Filters](https://gestrada.dev/posts/bloom-filter/), which covers the theory, math, benchmarks, and design decisions in depth.

## Features

- **Zero dependencies** — uses only the Go standard library (`hash/fnv`, `sync/atomic`, `sync`, `math`)
- **Zero allocations** — hashers are recycled via `sync.Pool` (0 B/op, 0 allocs/op)
- **Concurrent-safe with no locks** — `atomic.Uint64` bitset for lock-free `Add` and `Contains`
- **Kirsch-Mitzenmacher optimization** — one hash call simulates `k` independent hashes
- **Configurable** — create from explicit `(m, k)` or from `(n, p)` probability parameters
- **Pluggable hash function** — default FNV-1a, swappable via `WithHashFunc` option
- **Binary serialization** — `MarshalBinary` / `UnmarshalBinary` for persistence and network transfer

## Install

```sh
go get github.com/phrozen/bloom
```

## Usage

Bloom filters answer one question: *"Is this item in the set?"* The answer is either a definitive **No** or a probabilistic **Probably**. There are no false negatives — if the filter says no, the item was never added. This makes them ideal for guarding expensive lookups: check the filter first, skip the database/disk/network call when the answer is no.

### Create from probability parameters

Most of the time, you know how many items you expect and what false positive rate you can tolerate. The constructor handles the math for you — it computes the optimal number of bits and hash functions automatically:

```go
// Filter for 1M items with a 1% false positive rate.
// Internally this allocates ~1.14 MB (9.58M bits) and uses 7 hash functions.
f := bloom.NewFilterFromProbability(1_000_000, 0.01)
```

### Create with explicit parameters

If you already know the exact bit size and hash count you want (e.g. reproducing a filter from known parameters), you can specify them directly:

```go
// 10 million bits, 7 hash functions
f := bloom.NewFilter(10_000_000, 7)
```

### Add and query

Both functions are concurrent-safe and cannot fail (no error). Passing an empty `[]byte` is valid — it simply hashes like any other input.

```go
id := "550e8400-e29b-41d4-a716-446655440000"
f.Add([]byte(id))

if !f.Contains([]byte(id)) {
    // Definitively NOT in the set — skip the database entirely
    return ErrNotFound
}
// Probably in the set — check the database to confirm
```

### Custom hash function

The default hasher is FNV-1a from the standard library. If you need a different algorithm, swap it via the `WithHashFunc` option. Any `hash.Hash64` implementation works:

```go
import "github.com/cespare/xxhash/v2"

f := bloom.NewFilterFromProbability(1_000_000, 0.01,
    bloom.WithHashFunc(func() hash.Hash64 { return xxhash.New() }),
)
```

### Serialize / Deserialize

Filters implement `encoding.BinaryMarshaler` and `encoding.BinaryUnmarshaler`, so you can persist them to disk, send them over the network, or embed them in any encoding that supports those interfaces:

```go
// Save
data, err := f.MarshalBinary()

// Restore
var restored bloom.Filter
err = restored.UnmarshalBinary(data)
```

The binary format is compact: 16 bytes of header (`m` + `k`) followed by the raw bitset. A 1M-item filter at 1% FPR serializes to ~1.14 MB.

## Benchmarks

All benchmarks on AMD Ryzen 7 5800X (16 threads), 16-byte UUID keys, `go test -bench=. -benchmem`:

### Per-operation

| Operation | ns/op | B/op | allocs/op |
|---|---|---|---|
| `Add` | ~36 ns | 0 | 0 |
| `Contains` | ~25 ns | 0 | 0 |

### At scale (1M items, 1% FPR, 16 goroutines)

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| Concurrent Add | ~12 ns | 0 | 0 |
| Concurrent Contains | ~9 ns | 0 | 0 |
| Concurrent Add+Contains (50/50) | ~14 ns | 0 | 0 |
| **Filter memory footprint** | **1.14 MB** | | |
| **Actual false positive rate** | **~1.00%** | | |

### Hash function comparison

The `sync.Pool` optimization eliminates the interface allocation penalty entirely. All hashers now perform within the same ballpark, despite wildly different internal struct sizes:

| Hasher | Add ns/op | Contains ns/op | allocs/op |
|---|---|---|---|
| FNV-1a (default, std lib) | ~36 ns | ~25 ns | 0 |
| [xxHash](https://github.com/cespare/xxhash) | ~40 ns | ~26 ns | 0 |
| [Murmur3](https://github.com/twmb/murmur3) | ~36 ns | ~29 ns | 0 |
| [XXH3](https://github.com/zeebo/xxh3) | ~36 ns | ~29 ns | 0 |

FNV-1a is the default because it ships with Go — zero dependencies — and with Kirsch-Mitzenmacher splitting a 64-bit hash into two 32-bit halves, all four hashers produce identical false positive rates (~1% for a 1% target). Hash "quality" is a non-factor for this use case.

## How it works

1. **Bit array** — a `[]atomic.Uint64` slice aligned to CPU word size for single-instruction bit operations
2. **Kirsch-Mitzenmacher** — hash once, split into two 32-bit halves `h1` and `h2`, simulate `k` hashes via `h1 + i*h2` ([paper](https://www.eecs.harvard.edu/~michaelm/postscripts/rsa2008.pdf))
3. **sync.Pool** — recycles hasher instances so `Add`/`Contains` do zero heap allocations
4. **Atomic operations** — `Or` to set bits, `Load` to read them — fully concurrent, no mutex

## API

```go
// Constructors
func NewFilter(m int, k int, opts ...Option) *Filter
func NewFilterFromProbability(n int, p float64, opts ...Option) *Filter

// Options
func WithHashFunc(h HashFunc) Option

// Operations
func (f *Filter) Add(data []byte)
func (f *Filter) Contains(data []byte) bool

// Serialization (encoding.BinaryMarshaler / encoding.BinaryUnmarshaler)
func (f *Filter) MarshalBinary() ([]byte, error)
func (f *Filter) UnmarshalBinary(data []byte) error
```

## License

[Apache 2.0](LICENSE)