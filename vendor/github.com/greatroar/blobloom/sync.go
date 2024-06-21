// Copyright 2021-2022 the Blobloom authors
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

package blobloom

import "sync/atomic"

// A SyncFilter is a Bloom filter that can be accessed and updated
// by multiple goroutines concurrently.
//
// A SyncFilter mostly behaves as a regular filter protected by a lock,
//
//	type SyncFilter struct {
//		Filter
//		lock sync.Mutex
//	}
//
// with each method taking and releasing the lock,
// but is implemented much more efficiently.
// See the method descriptions for exceptions to the previous rule.
type SyncFilter struct {
	b []block // Shards.
	k int     // Number of hash functions required.
}

// NewSync constructs a Bloom filter with given numbers of bits and hash functions.
//
// The number of bits should be at least BlockBits; smaller values are silently
// increased.
//
// The number of hashes reflects the number of hashes synthesized from the
// single hash passed in by the client. It is silently increased to two if
// a lower value is given.
func NewSync(nbits uint64, nhashes int) *SyncFilter {
	nbits, nhashes = fixBitsAndHashes(nbits, nhashes)

	return &SyncFilter{
		b: make([]block, nbits/BlockBits),
		k: nhashes,
	}

}

// Add insert a key with hash value h into f.
func (f *SyncFilter) Add(h uint64) {
	h1, h2 := uint32(h>>32), uint32(h)
	b := getblock(f.b, h2)

	for i := 1; i < f.k; i++ {
		h1, h2 = doublehash(h1, h2, i)
		setbitAtomic(b, h1)
	}
}

// Cardinality estimates the number of distinct keys added to f.
//
// The estimate is most reliable when f is filled to roughly its capacity.
// It gets worse as f gets more densely filled. When one of the blocks is
// entirely filled, the estimate becomes +Inf.
//
// The return value is the maximum likelihood estimate of Papapetrou, Siberski
// and Nejdl, summed over the blocks
// (https://www.win.tue.nl/~opapapetrou/papers/Bloomfilters-DAPD.pdf).
//
// If other goroutines are concurrently adding keys,
// the estimate may lie in between what would have been returned
// before the concurrent updates started and what is returned
// after the updates complete.
func (f *SyncFilter) Cardinality() float64 {
	return cardinality(f.k, f.b, onescountAtomic)
}

// Empty reports whether f contains no keys.
//
// If other goroutines are concurrently adding keys,
// Empty may return a false positive.
func (f *SyncFilter) Empty() bool {
	for i := 0; i < len(f.b); i++ {
		for j := 0; j < blockWords; j++ {
			if atomic.LoadUint32(&f.b[i][j]) != 0 {
				return false
			}
		}
	}
	return true
}

// Fill sets f to a completely full filter.
// After Fill, Has returns true for any key.
func (f *SyncFilter) Fill() {
	for i := 0; i < len(f.b); i++ {
		for j := 0; j < blockWords; j++ {
			atomic.StoreUint32(&f.b[i][j], ^uint32(0))
		}
	}
}

// Has reports whether a key with hash value h has been added.
// It may return a false positive.
func (f *SyncFilter) Has(h uint64) bool {
	h1, h2 := uint32(h>>32), uint32(h)
	b := getblock(f.b, h2)

	for i := 1; i < f.k; i++ {
		h1, h2 = doublehash(h1, h2, i)
		if !getbitAtomic(b, h1) {
			return false
		}
	}
	return true
}

// getbitAtomic reports whether bit (i modulo BlockBits) is set.
func getbitAtomic(b *block, i uint32) bool {
	bit := uint32(1) << (i % wordSize)
	x := atomic.LoadUint32(&(*b)[(i/wordSize)%blockWords])
	return x&bit != 0
}

// setbit sets bit (i modulo BlockBits) of b, atomically.
func setbitAtomic(b *block, i uint32) {
	bit := uint32(1) << (i % wordSize)
	p := &(*b)[(i/wordSize)%blockWords]

	for {
		old := atomic.LoadUint32(p)
		if old&bit != 0 {
			// Checking here instead of checking the return value from
			// the CAS is between 50% and 80% faster on the benchmark.
			return
		}
		atomic.CompareAndSwapUint32(p, old, old|bit)
	}
}
