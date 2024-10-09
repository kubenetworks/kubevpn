// Copyright 2023 the Blobloom authors
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

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
)

const maxCommentLen = 44

// Dump writes f to w, with an optional comment string, in the binary format
// that a Loader accepts. It returns the number of bytes written to w.
//
// The comment may contain arbitrary data, within the limits layed out by the
// format description. It can be used to record the hash function to be used
// with a Filter.
func Dump(w io.Writer, f *Filter, comment string) (int64, error) {
	return dump(w, f.b, f.k, comment)
}

// DumpSync is like Dump, but for SyncFilters.
//
// If other goroutines are simultaneously modifying f,
// their modifications may not be reflected in the dump.
// Separate synchronization is required to prevent this.
//
// The format produced is the same as Dump's. The fact that
// the argument is a SyncFilter is not encoded in the dump.
func DumpSync(w io.Writer, f *SyncFilter, comment string) (n int64, err error) {
	return dump(w, f.b, f.k, comment)
}

func dump(w io.Writer, b []block, nhashes int, comment string) (n int64, err error) {
	switch {
	case len(b) == 0 || nhashes == 0:
		err = errors.New("blobloom: won't dump uninitialized Filter")
	case len(comment) > maxCommentLen:
		err = fmt.Errorf("blobloom: comment of length %d too long", len(comment))
	case strings.IndexByte(comment, 0) != -1:
		err = fmt.Errorf("blobloom: comment %q contains zero byte", len(comment))
	}
	if err != nil {
		return 0, err
	}

	var buf [64]byte
	copy(buf[:8], "blobloom")
	// As documented in the comment for Loader, we store one less than the
	// number of blocks. This way, we can use the otherwise invalid value 0
	// and store 2³² blocks instead of at most 2³²-1.
	binary.LittleEndian.PutUint32(buf[12:], uint32(len(b)-1))
	binary.LittleEndian.PutUint32(buf[16:], uint32(nhashes))
	copy(buf[20:], comment)

	k, err := w.Write(buf[:])
	n = int64(k)
	if err != nil {
		return n, err
	}

	for i := range b {
		for j := range b[i] {
			x := atomic.LoadUint32(&b[i][j])
			binary.LittleEndian.PutUint32(buf[4*j:], x)
		}
		k, err = w.Write(buf[:])
		n += int64(k)
		if err != nil {
			break
		}
	}

	return n, err
}

// A Loader reads a Filter or SyncFilter from an io.Reader.
//
// A Loader accepts the binary format produced by Dump. The format starts
// with a 64-byte header:
//   - the string "blobloom", in ASCII;
//   - a four-byte version number, which must be zero;
//   - the number of Bloom filter blocks, minus one, as a 32-bit integer;
//   - the number of hashes, as a 32-bit integer;
//   - a comment of at most 44 non-zero bytes, padded to 44 bytes with zeros.
//
// After the header come the 512-bit blocks, divided into sixteen 32-bit limbs.
// All integers are little-endian.
type Loader struct {
	buf [64]byte
	r   io.Reader
	err error

	Comment string // Comment field. Filled in by NewLoader.
	nblocks uint64
	nhashes int
}

// NewLoader parses the format header from r and returns a Loader
// that can be used to load a Filter from it.
func NewLoader(r io.Reader) (*Loader, error) {
	l := &Loader{r: r}

	err := l.fillbuf()
	if err != nil {
		return nil, err
	}

	version := binary.LittleEndian.Uint32(l.buf[8:])
	// See comment in dump for the +1.
	l.nblocks = 1 + uint64(binary.LittleEndian.Uint32(l.buf[12:]))
	l.nhashes = int(binary.LittleEndian.Uint32(l.buf[16:]))
	comment := l.buf[20:]

	switch {
	case string(l.buf[:8]) != "blobloom":
		err = errors.New("blobloom: not a Bloom filter dump")
	case version != 0:
		err = errors.New("blobloom: unsupported dump version")
	case l.nhashes == 0:
		err = errors.New("blobloom: zero hashes in Bloom filter dump")
	}
	if err == nil {
		comment, err = checkComment(comment)
		l.Comment = string(comment)
	}

	if err != nil {
		l = nil
	}
	return l, err
}

// Load sets f to the union of f and the Loader's filter, then returns f.
// If f is nil, a new Filter of the appropriate size is constructed.
//
// If f is not nil and an error occurs while reading from the Loader,
// f may end up in an inconsistent state.
func (l *Loader) Load(f *Filter) (*Filter, error) {
	if f == nil {
		nbits := BlockBits * l.nblocks
		if nbits > MaxBits {
			return nil, fmt.Errorf("blobloom: %d blocks is too large", l.nblocks)
		}
		f = New(nbits, int(l.nhashes))
	} else if err := l.checkBitsAndHashes(len(f.b), f.k); err != nil {
		return nil, err
	}

	for i := range f.b {
		if err := l.fillbuf(); err != nil {
			return nil, err
		}

		for j := range f.b[i] {
			f.b[i][j] |= binary.LittleEndian.Uint32(l.buf[4*j:])
		}
	}

	return f, nil
}

// Load sets f to the union of f and the Loader's filter, then returns f.
// If f is nil, a new SyncFilter of the appropriate size is constructed.
// Else, LoadSync may run concurrently with other modifications to f.
//
// If f is not nil and an error occurs while reading from the Loader,
// f may end up in an inconsistent state.
func (l *Loader) LoadSync(f *SyncFilter) (*SyncFilter, error) {
	if f == nil {
		nbits := BlockBits * l.nblocks
		if nbits > MaxBits {
			return nil, fmt.Errorf("blobloom: %d blocks is too large", l.nblocks)
		}
		f = NewSync(nbits, int(l.nhashes))
	} else if err := l.checkBitsAndHashes(len(f.b), f.k); err != nil {
		return nil, err
	}

	for i := range f.b {
		if err := l.fillbuf(); err != nil {
			return nil, err
		}

		for j := range f.b[i] {
			p := &f.b[i][j]
			x := binary.LittleEndian.Uint32(l.buf[4*j:])

			for {
				old := atomic.LoadUint32(p)
				if atomic.CompareAndSwapUint32(p, old, old|x) {
					break
				}
			}
		}
	}

	return f, nil
}

func (l *Loader) checkBitsAndHashes(nblocks, nhashes int) error {
	switch {
	case nblocks != int(l.nblocks):
		return fmt.Errorf("blobloom: Filter has %d blocks, but dump has %d", nblocks, l.nblocks)
	case nhashes != l.nhashes:
		return fmt.Errorf("blobloom: Filter has %d hashes, but dump has %d", nhashes, l.nhashes)
	}
	return nil
}

func (l *Loader) fillbuf() error {
	_, err := io.ReadFull(l.r, l.buf[:])
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func checkComment(p []byte) ([]byte, error) {
	eos := bytes.IndexByte(p, 0)
	if eos != -1 {
		tail := p[eos+1:]
		if !bytes.Equal(tail, make([]byte, len(tail))) {
			return nil, fmt.Errorf("blobloom: comment block %q contains zero byte", p)
		}
		p = p[:eos]
	}
	return p, nil
}
