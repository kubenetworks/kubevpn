Blobloom
========

A Bloom filter package for Go (golang) with no compile-time dependencies.

This package implements a version of Bloom filters called [blocked Bloom filters](
https://algo2.iti.kit.edu/documents/cacheefficientbloomfilters-jea.pdf),
which get a speed boost from using the CPU cache more efficiently
than regular Bloom filters.

Unlike most Bloom filter packages for Go,
this one doesn't run a hash function for you.
That's a benefit if you need a custom hash
or you want pick the fastest one for an application.

Usage
-----

To construct a Bloom filter, you need to know how many keys you want to store
and what rate of false positives you find acceptable.

	f := blobloom.NewOptimized(blobloom.Config{
		Capacity: nkeys, // Expected number of keys.
		FPRate:   1e-4,  // Accept one false positive per 10,000 lookups.
	})

To add a key:

	// import "github.com/cespare/xxhash/v2"
	f.Add(xxhash.Sum64(key))

To test for the presence of a key in the filter:

	if f.Has(xxhash.Sum64(key)) {
		// Key is probably in f.
	} else {
		// Key is certainly not in f.
	}

The false positive rate is defined as usual:
if you look up 10,000 random keys in a Bloom filter filled to capacity,
an expected one of those is a false positive for FPRate 1e-4.

See the examples/ directory and the
[package documentation](https://pkg.go.dev/github.com/greatroar/blobloom)
for further usage information and examples.

Hash functions
--------------

Blobloom does not provide hash functions. Instead, it requires client code to
represent each key as a single 64-bit hash value, leaving it to the user to
pick the right hash function for a particular problem. Here are some general
suggestions:

* If you use Bloom filters to speed up access to a key-value store, you might
want to look at [xxh3](https://github.com/zeebo/xxh3) or [xxhash](
https://github.com/cespare/xxhash).
* If your keys are cryptographic hashes, consider using the first 8 bytes of those hashes.
* If you use Bloom filters to make probabilistic decisions, a randomized hash
function such as [maphash](https://golang.org/pkg/hash/maphash) should prevent
the same false positives occurring every time.

When evaluating a hash function, or designing a custom one,
make sure it is a 64-bit hash that properly mixes its input bits.
Casting a 32-bit hash to uint64 gives suboptimal results.
So does passing integer keys in without running them through a mixing function.



License
-------

Copyright © 2020-2023 the Blobloom authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
