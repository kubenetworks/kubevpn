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

//go:build (!amd64 && !arm64) || nounsafe
// +build !amd64,!arm64 nounsafe

package blobloom

import (
	"math/bits"
	"sync/atomic"
)

func (f *Filter) intersect(g *Filter) {
	for i := range f.b {
		f.b[i].intersect(&g.b[i])
	}
}

func (f *Filter) union(g *Filter) {
	for i := range f.b {
		f.b[i].union(&g.b[i])
	}
}

func (b *block) intersect(c *block) {
	b[0] &= c[0]
	b[1] &= c[1]
	b[2] &= c[2]
	b[3] &= c[3]
	b[4] &= c[4]
	b[5] &= c[5]
	b[6] &= c[6]
	b[7] &= c[7]
	b[8] &= c[8]
	b[9] &= c[9]
	b[10] &= c[10]
	b[11] &= c[11]
	b[12] &= c[12]
	b[13] &= c[13]
	b[14] &= c[14]
	b[15] &= c[15]
}

func (b *block) union(c *block) {
	b[0] |= c[0]
	b[1] |= c[1]
	b[2] |= c[2]
	b[3] |= c[3]
	b[4] |= c[4]
	b[5] |= c[5]
	b[6] |= c[6]
	b[7] |= c[7]
	b[8] |= c[8]
	b[9] |= c[9]
	b[10] |= c[10]
	b[11] |= c[11]
	b[12] |= c[12]
	b[13] |= c[13]
	b[14] |= c[14]
	b[15] |= c[15]
}

func onescount(b *block) (n int) {
	n += bits.OnesCount32(b[0])
	n += bits.OnesCount32(b[1])
	n += bits.OnesCount32(b[2])
	n += bits.OnesCount32(b[3])
	n += bits.OnesCount32(b[4])
	n += bits.OnesCount32(b[5])
	n += bits.OnesCount32(b[6])
	n += bits.OnesCount32(b[7])
	n += bits.OnesCount32(b[8])
	n += bits.OnesCount32(b[9])
	n += bits.OnesCount32(b[10])
	n += bits.OnesCount32(b[11])
	n += bits.OnesCount32(b[12])
	n += bits.OnesCount32(b[13])
	n += bits.OnesCount32(b[14])
	n += bits.OnesCount32(b[15])

	return n
}

func onescountAtomic(b *block) (n int) {
	n += bits.OnesCount32(atomic.LoadUint32(&b[0]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[1]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[2]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[3]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[4]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[5]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[6]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[7]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[8]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[9]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[10]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[11]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[12]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[13]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[14]))
	n += bits.OnesCount32(atomic.LoadUint32(&b[15]))

	return n
}
