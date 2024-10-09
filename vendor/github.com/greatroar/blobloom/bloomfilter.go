// Copyright 2020-2022 the Blobloom authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package blobloom implements blocked Bloom filters.
//
// Blocked Bloom filters are an approximate set data structure: if a key has
// been added to a filter, a lookup of that key returns true, but if the key
// has not been added, there is a non-zero probability that the lookup still
// returns true (a false positive). False negatives are impossible: if the
// lookup for a key returns false, that key has not been added.
//
// In this package, keys are represented exclusively as hashes. Client code
// is responsible for supplying a 64-bit hash value.
//
// Compared to standard Bloom filters, blocked Bloom filters use the CPU
// cache more efficiently. A blocked Bloom filter is an array of ordinary
// Bloom filters of fixed size BlockBits (the blocks). The lower half of the
// hash selects the block to use.
//
// To achieve the same false positive rate (FPR) as a standard Bloom filter,
// a blocked Bloom filter requires more memory. For an FPR of at most 2e-6
// (two in a million), it uses ~20% more memory. At 1e-10, the space required
// is double that of standard Bloom filter.
//
// For more details, see the 2010 paper by Putze, Sanders and Singler,
// https://algo2.iti.kit.edu/documents/cacheefficientbloomfilters-jea.pdf.
package blobloom

import "math"

// BlockBits is the number of bits per block and the minimum number of bits
// in a Filter.
//
// The value of this constant is chosen to match the L1 cache line size
// of popular architectures (386, amd64, arm64).
const BlockBits = 512

// MaxBits is the maximum number of bits supported by a Filter.
const MaxBits = BlockBits << 32 // 256GiB.

// A Filter is a blocked Bloom filter.
type Filter struct {
	b []block // Shards.
	k int     // Number of hash functions required.
}

// New constructs a Bloom filter with given numbers of bits and hash functions.
//
// The number of bits should be at least BlockBits; smaller values are silently
// increased.
//
// The number of hashes reflects the number of hashes synthesized from the
// single hash passed in by the client. It is silently increased to two if
// a lower value is given.
func New(nbits uint64, nhashes int) *Filter {
	nbits, nhashes = fixBitsAndHashes(nbits, nhashes)

	return &Filter{
		b: make([]block, nbits/BlockBits),
		k: nhashes,
	}
}

func fixBitsAndHashes(nbits uint64, nhashes int) (uint64, int) {
	if nbits < 1 {
		nbits = BlockBits
	}
	if nhashes < 2 {
		nhashes = 2
	}
	if nbits > MaxBits {
		panic("nbits exceeds MaxBits")
	}

	// Round nbits up to a multiple of BlockBits.
	if nbits%BlockBits != 0 {
		nbits += BlockBits - nbits%BlockBits
	}

	return nbits, nhashes
}

// Add insert a key with hash value h into f.
func (f *Filter) Add(h uint64) {
	h1, h2 := uint32(h>>32), uint32(h)
	b := getblock(f.b, h2)

	for i := 1; i < f.k; i++ {
		h1, h2 = doublehash(h1, h2, i)
		b.setbit(h1)
	}
}

// log(1 - 1/BlockBits) computed with 128 bits precision.
// Note that this is extremely close to -1/BlockBits,
// which is what Wikipedia would have us use:
// https://en.wikipedia.org/wiki/Bloom_filter#Approximating_the_number_of_items_in_a_Bloom_filter.
const log1minus1divBlockbits = -0.0019550348358033505576274922418668121377

// Cardinality estimates the number of distinct keys added to f.
//
// The estimate is most reliable when f is filled to roughly its capacity.
// It gets worse as f gets more densely filled. When one of the blocks is
// entirely filled, the estimate becomes +Inf.
//
// The return value is the maximum likelihood estimate of Papapetrou, Siberski
// and Nejdl, summed over the blocks
// (https://www.win.tue.nl/~opapapetrou/papers/Bloomfilters-DAPD.pdf).
func (f *Filter) Cardinality() float64 {
	return cardinality(f.k, f.b, onescount)
}

func cardinality(nhashes int, b []block, onescount func(*block) int) float64 {
	k := float64(nhashes - 1)

	// The probability of some bit not being set in a single insertion is
	// p0 = (1-1/BlockBits)^k.
	//
	// logProb0Inv = 1 / log(p0) = 1 / (k*log(1-1/BlockBits)).
	logProb0Inv := 1 / (k * log1minus1divBlockbits)

	var n float64
	for i := range b {
		ones := onescount(&b[i])
		if ones == 0 {
			continue
		}
		n += math.Log1p(-float64(ones) / BlockBits)
	}
	return n * logProb0Inv
}

// Clear resets f to its empty state.
func (f *Filter) Clear() {
	for i := 0; i < len(f.b); i++ {
		f.b[i] = block{}
	}
}

// Empty reports whether f contains no keys.
func (f *Filter) Empty() bool {
	for i := 0; i < len(f.b); i++ {
		if f.b[i] != (block{}) {
			return false
		}
	}
	return true
}

// Equals returns true if f and g contain the same keys (in terms of Has)
// when used with the same hash function.
func (f *Filter) Equals(g *Filter) bool {
	if g.k != f.k || len(g.b) != len(f.b) {
		return false
	}
	for i := range g.b {
		if f.b[i] != g.b[i] {
			return false
		}
	}
	return true
}

// Fill set f to a completely full filter.
// After Fill, Has returns true for any key.
func (f *Filter) Fill() {
	for i := 0; i < len(f.b); i++ {
		for j := 0; j < blockWords; j++ {
			f.b[i][j] = ^uint32(0)
		}
	}
}

// Has reports whether a key with hash value h has been added.
// It may return a false positive.
func (f *Filter) Has(h uint64) bool {
	h1, h2 := uint32(h>>32), uint32(h)
	b := getblock(f.b, h2)

	for i := 1; i < f.k; i++ {
		h1, h2 = doublehash(h1, h2, i)
		if !b.getbit(h1) {
			return false
		}
	}
	return true
}

// doublehash generates the hash values to use in iteration i of
// enhanced double hashing from the values h1, h2 of the previous iteration.
// See https://www.ccs.neu.edu/home/pete/pub/bloom-filters-verification.pdf.
func doublehash(h1, h2 uint32, i int) (uint32, uint32) {
	h1 = h1 + h2
	h2 = h2 + uint32(i)
	return h1, h2
}

// NumBits returns the number of bits of f.
func (f *Filter) NumBits() uint64 {
	return BlockBits * uint64(len(f.b))
}

func checkBinop(f, g *Filter) {
	if len(f.b) != len(g.b) {
		panic("Bloom filters do not have the same number of bits")
	}
	if f.k != g.k {
		panic("Bloom filters do not have the same number of hash functions")
	}
}

// Intersect sets f to the intersection of f and g.
//
// Intersect panics when f and g do not have the same number of bits and
// hash functions. Both Filters must be using the same hash function(s),
// but Intersect cannot check this.
//
// Since Bloom filters may return false positives, Has may return true for
// a key that was not in both f and g.
//
// After Intersect, the estimates from f.Cardinality and f.FPRate should be
// considered unreliable.
func (f *Filter) Intersect(g *Filter) {
	checkBinop(f, g)
	f.intersect(g)
}

// Union sets f to the union of f and g.
//
// Union panics when f and g do not have the same number of bits and
// hash functions. Both Filters must be using the same hash function(s),
// but Union cannot check this.
func (f *Filter) Union(g *Filter) {
	checkBinop(f, g)
	f.union(g)
}

const (
	wordSize   = 32
	blockWords = BlockBits / wordSize
)

// A block is a fixed-size Bloom filter, used as a shard of a Filter.
type block [blockWords]uint32

func getblock(b []block, h2 uint32) *block {
	i := reducerange(h2, uint32(len(b)))
	return &b[i]
}

// reducerange maps i to an integer in the range [0,n).
// https://lemire.me/blog/2016/06/27/a-fast-alternative-to-the-modulo-reduction/
func reducerange(i, n uint32) uint32 {
	return uint32((uint64(i) * uint64(n)) >> 32)
}

// getbit reports whether bit (i modulo BlockBits) is set.
func (b *block) getbit(i uint32) bool {
	bit := uint32(1) << (i % wordSize)
	x := (*b)[(i/wordSize)%blockWords] & bit
	return x != 0
}

// setbit sets bit (i modulo BlockBits) of b.
func (b *block) setbit(i uint32) {
	bit := uint32(1) << (i % wordSize)
	(*b)[(i/wordSize)%blockWords] |= bit
}
