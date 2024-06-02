// Copyright (C) 2024 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Package result provides the result type for ignore matching. This is a
// separate package in order to break import cycles.
package ignoreresult

const (
	NotIgnored R = 0
	// `Ignored` is defined in platform specific files
	IgnoredDeletable = Ignored | deletableBit
	IgnoreAndSkip    = Ignored | canSkipDirBit
)

const (
	// Private definitions of the bits that make up the result value
	ignoreBit R = 1 << iota
	deletableBit
	foldCaseBit
	canSkipDirBit
)

type R uint8

// IsIgnored returns true if the result is ignored.
func (r R) IsIgnored() bool {
	return r&ignoreBit != 0
}

// IsDeletable returns true if the result is ignored and deletable.
func (r R) IsDeletable() bool {
	return r.IsIgnored() && r&deletableBit != 0
}

// IsCaseFolded returns true if the result was a case-insensitive match.
func (r R) IsCaseFolded() bool {
	return r&foldCaseBit != 0
}

// CanSkipDir returns true if the result is ignored and the directory can be
// skipped (no need to recurse deeper). Note that ignore matches are textual
// and based on the name only -- this being true does not mean that the
// matched item is a directory, merely that *if* it is a directory, it can
// be skipped.
func (r R) CanSkipDir() bool {
	return r.IsIgnored() && r&canSkipDirBit != 0
}

// ToggleIgnored returns a copy of the result with the ignored bit toggled.
func (r R) ToggleIgnored() R {
	return r ^ ignoreBit
}

// WithDeletable returns a copy of the result with the deletable bit set.
func (r R) WithDeletable() R {
	return r | deletableBit
}

// WithFoldCase returns a copy of the result with the fold case bit set.
func (r R) WithFoldCase() R {
	return r | foldCaseBit
}

// WithSkipDir returns a copy of the result with the skip dir bit set.
func (r R) WithSkipDir() R {
	return r | canSkipDirBit
}

// String returns a human readable representation of the result flags.
func (r R) String() string {
	var s string
	if r&ignoreBit != 0 {
		s += "i"
	} else {
		s += "-"
	}
	if r&deletableBit != 0 {
		s += "d"
	} else {
		s += "-"
	}
	if r&foldCaseBit != 0 {
		s += "f"
	} else {
		s += "-"
	}
	if r&canSkipDirBit != 0 {
		s += "s"
	} else {
		s += "-"
	}
	return s
}
