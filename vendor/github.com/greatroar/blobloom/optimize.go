// Copyright 2020 the Blobloom authors
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

import "math"

// A Config holds parameters for Optimize or NewOptimized.
type Config struct {
	// Capacity is the expected number of distinct keys to be added.
	// More keys can always be added, but the false positive rate can be
	// expected to drop below FPRate if their number exceeds the Capacity.
	Capacity uint64

	// Desired lower bound on the false positive rate when the Bloom filter
	// has been filled to its capacity. FPRate must be between zero
	// (exclusive) and one (inclusive).
	FPRate float64

	// Maximum size of the Bloom filter in bits. Zero means the global
	// MaxBits constant. A value less than BlockBits means BlockBits.
	MaxBits uint64

	// Trigger the "contains filtered or unexported fields" message for
	// forward compatibility and force the caller to use named fields.
	_ struct{}
}

// NewOptimized is shorthand for New(Optimize(config)).
func NewOptimized(config Config) *Filter {
	return New(Optimize(config))
}

// NewSyncOptimized is shorthand for New(Optimize(config)).
func NewSyncOptimized(config Config) *SyncFilter {
	return NewSync(Optimize(config))
}

// Optimize returns numbers of keys and hash functions that achieve the
// desired false positive described by config.
//
// Optimize panics when config.FPRate is invalid.
//
// The estimated number of bits is imprecise for false positives rates below
// ca. 1e-15.
func Optimize(config Config) (nbits uint64, nhashes int) {
	n := float64(config.Capacity)
	p := config.FPRate

	if p <= 0 || p > 1 {
		panic("false positive rate for a Bloom filter must be > 0, <= 1")
	}
	if n == 0 {
		// Assume the client wants to add at least one key; log2(0) = -inf.
		n = 1
	}

	// The optimal nbits/n is c = -log2(p) / ln(2) for a vanilla Bloom filter.
	c := math.Ceil(-math.Log2(p) / math.Ln2)
	if c < float64(len(correctC)) {
		c = float64(correctC[int(c)])
	} else {
		// We can't achieve the desired FPR. Just triple the number of bits.
		c *= 3
	}
	nbits = uint64(c * n)

	// Round up to a multiple of BlockBits.
	if nbits%BlockBits != 0 {
		nbits += BlockBits - nbits%BlockBits
	}

	var maxbits uint64 = MaxBits
	if config.MaxBits != 0 && config.MaxBits < maxbits {
		maxbits = config.MaxBits
		if maxbits < BlockBits {
			maxbits = BlockBits
		}
	}
	if nbits > maxbits {
		nbits = maxbits
		// Round down to a multiple of BlockBits.
		nbits -= nbits % BlockBits
	}

	// The corresponding optimal number of hash functions is k = c * log(2).
	// Try rounding up and down to see which rounding is better.
	c = float64(nbits) / n
	k := c * math.Ln2
	if k < 1 {
		nhashes = 1
		return nbits, nhashes
	}

	ceilK, floorK := math.Floor(k), math.Ceil(k)
	if ceilK == floorK {
		return nbits, int(ceilK)
	}

	fprCeil, _ := fpRate(c, math.Ceil(k))
	fprFloor, _ := fpRate(c, math.Floor(k))
	if fprFloor < fprCeil {
		k = floorK
	} else {
		k = ceilK
	}

	return nbits, int(k)
}

// correctC maps c = m/n for a vanilla Bloom filter to the c' for a
// blocked Bloom filter.
//
// This is Putze et al.'s Table I, extended down to zero.
// For c > 34, the values become huge and are hard to compute.
var correctC = []byte{
	1, 1, 2, 4, 5,
	6, 7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 18, 20, 21, 23,
	25, 26, 28, 30, 32, 35, 38, 40, 44, 48, 51, 58, 64, 74, 90,
}

// FPRate computes an estimate of the false positive rate of a Bloom filter
// after nkeys distinct keys have been added.
func FPRate(nkeys, nbits uint64, nhashes int) float64 {
	if nkeys == 0 {
		return 0
	}
	p, _ := fpRate(float64(nbits)/float64(nkeys), float64(nhashes))
	return p
}

func fpRate(c, k float64) (p float64, iter int) {
	switch {
	case c == 0:
		panic("0 bits per key is too few")
	case k == 0:
		panic("0 hashes is too few")
	}

	// Putze et al.'s Equation (3).
	//
	// The Poisson distribution has a single spike around its mean
	// BlockBits/c that gets slimmer and further away from zero as c tends
	// to zero (the Bloom filter gets more filled). We start at the mean,
	// then add terms left and right of it until their relative contribution
	// drops below ε.
	const ε = 1e-9
	mean := BlockBits / c

	// Ceil to make sure we start at one, not zero.
	i := math.Ceil(mean)
	p = math.Exp(logPoisson(mean, i) + logFprBlock(BlockBits/i, k))

	for j := i - 1; j > 0; j-- {
		add := math.Exp(logPoisson(mean, j) + logFprBlock(BlockBits/j, k))
		p += add
		iter++
		if add/p < ε {
			break
		}
	}

	for j := i + 1; ; j++ {
		add := math.Exp(logPoisson(mean, j) + logFprBlock(BlockBits/j, k))
		p += add
		iter++
		if add/p < ε {
			break
		}
	}

	return p, iter
}

// FPRate computes an estimate of f's false positive rate after nkeys distinct
// keys have been added.
func (f *Filter) FPRate(nkeys uint64) float64 {
	return FPRate(nkeys, f.NumBits(), f.k)
}

// Log of the FPR of a single block, FPR = (1 - exp(-k/c))^k.
func logFprBlock(c, k float64) float64 {
	return k * math.Log1p(-math.Exp(-k/c))
}

// Log of the Poisson distribution's pmf.
func logPoisson(λ, k float64) float64 {
	lg, _ := math.Lgamma(k + 1)
	return k*math.Log(λ) - λ - lg
}
