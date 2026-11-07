package wire

import (
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
)

// Stream obfuscation for the gtcp data plane. When enabled, the raw byte
// stream between the TLS record layer and the datagram framing is XORed with
// a per-connection keystream: the dialer prepends a 16-byte random seed to
// its first write, and both directions derive independent keystreams from it.
// Every frame — no matter which producer stamps it (wire.Conn, BatchWriter,
// route writes, bufferedTCP) — crosses the wrapper, so the transport looks
// like TLS records of uniformly random bytes instead of a static
// [length][type][packet] signature.
//
// This is obfuscation, not confidentiality: the seed travels in the clear and
// the keystream is not cryptographic. Confidentiality stays with TLS; record
// size and timing fingerprints are out of scope here (addressed by the
// TLS-mimicry follow-up stage).

// obfSeedLen is the per-connection seed exchanged (in the clear) as the first
// bytes written by the dialer.
const obfSeedLen = 16

var obfuscationEnabled atomic.Bool

// SetObfuscation enables or disables stream obfuscation for connections
// established afterwards. Both endpoints must agree (they derive the setting
// from the same secret/env); a mismatch garbles the stream.
func SetObfuscation(on bool) { obfuscationEnabled.Store(on) }

// ObfuscationEnabled reports whether stream obfuscation is enabled.
func ObfuscationEnabled() bool { return obfuscationEnabled.Load() }

var errObfuscationWriteBeforeSeed = errors.New("obfuscation: acceptor wrote before reading the seed")

// ObfuscateDial wraps a freshly dialed connection with the obfuscating
// stream. The seed is generated now and transmitted lazily with the first
// write, so the constructor performs no I/O.
func ObfuscateDial(conn net.Conn) net.Conn {
	var seed [obfSeedLen]byte
	if _, err := crand.Read(seed[:]); err != nil {
		// A failed crypto/rand read leaves the seed deterministic only in a
		// catastrophically broken runtime; fall back to a weak seed rather
		// than dropping obfuscation entirely.
		binary.BigEndian.PutUint64(seed[:8], rand.Uint64())
		binary.BigEndian.PutUint64(seed[8:], rand.Uint64())
	}
	c := &obfuscatedConn{Conn: conn, dialer: true, seed: seed}
	c.initStreams()
	return c
}

// ObfuscateAccept wraps an accepted connection with the obfuscating stream.
// The seed is consumed lazily from the first read; writing before any read
// (impossible in the gtcp protocol, where the client always speaks first)
// returns errObfuscationWriteBeforeSeed.
func ObfuscateAccept(conn net.Conn) net.Conn {
	return &obfuscatedConn{Conn: conn}
}

// obfuscatedConn XORs every byte crossing the underlying connection with the
// direction's keystream. The keystreams advance in lockstep on both peers
// because TCP delivers each direction's bytes in order and exactly once.
type obfuscatedConn struct {
	net.Conn
	dialer bool

	writeMu sync.Mutex
	readMu  sync.Mutex
	seed    [obfSeedLen]byte
	wStream obfKeystream
	rStream obfKeystream
	wSent   bool // seed prepended to a write (dialer only)
	rReady  bool // seed consumed from the stream
	scratch []byte
}

// initStreams derives the two directional keystreams from the seed. Direction
// 0 is dialer→acceptor, direction 1 the reverse; both peers agree because the
// dialer always originates the seed.
func (c *obfuscatedConn) initStreams() {
	base := binary.BigEndian.Uint64(c.seed[:8]) ^ binary.BigEndian.Uint64(c.seed[8:])
	if c.dialer {
		c.wStream, c.rStream = obfNewStream(base, 0), obfNewStream(base, 1)
	} else {
		c.wStream, c.rStream = obfNewStream(base, 1), obfNewStream(base, 0)
	}
	c.rReady = true
}

func (c *obfuscatedConn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if !c.rReady {
		var seed [obfSeedLen]byte
		if _, err := io.ReadFull(c.Conn, seed[:]); err != nil {
			return 0, err
		}
		c.seed = seed
		c.initStreams()
	}
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.rStream.xor(b[:n])
	}
	return n, err
}

func (c *obfuscatedConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !c.rReady {
		// The acceptor has not read the dialer's seed yet; the gtcp protocol
		// never writes server-side before a client frame arrives.
		return 0, errObfuscationWriteBeforeSeed
	}
	prefix := 0
	if c.dialer && !c.wSent {
		// Only the dialer transmits the seed, with (and ahead of) its first
		// payload. The acceptor's writes are ordinary: by the time it writes,
		// it has consumed the dialer's seed on the read path.
		prefix = obfSeedLen
		c.wSent = true
	}
	total := prefix + len(b)
	if cap(c.scratch) < total {
		c.scratch = make([]byte, total)
	}
	buf := c.scratch[:total]
	if prefix > 0 {
		copy(buf, c.seed[:])
	}
	// Fuse the copy and the XOR into one pass; the caller's buffer is never
	// touched (framing reuses pooled buffers).
	c.wStream.xorCopy(buf[prefix:], b)
	written, err := WriteFull(c.Conn, buf)
	// WriteFull loops until the whole buffer is out or the stream errors; a
	// partial write with error leaves the stream damaged and the caller must
	// close the connection (the shared framing contract), so keystream
	// desynchronization cannot be observed on a live connection.
	return written - min(written, prefix), err
}

// obfKeystream is a per-direction xorshift64* keystream seeded from the
// connection seed. Word-wise generation keeps the transform under half a
// nanosecond per byte. NOT cryptographic — obfuscation only.
type obfKeystream struct {
	state uint64
	word  [8]byte // current keystream word, consumed low byte first
	rem   int     // unconsumed bytes in word (0..7)
}

func obfNewStream(base uint64, dir int) obfKeystream {
	return obfKeystream{state: base ^ uint64(dir)*0x9E3779B97F4A7C15}
}

// next advances the xorshift64* state and returns the next keystream word.
func (s *obfKeystream) next() uint64 {
	x := s.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	s.state = x
	return x * 0x2545F4914F6CDD1D
}

// xor XORs b in place. Partial-word leftovers are consumed before generating
// further words so the keystream position stays a byte count.
func (s *obfKeystream) xor(b []byte) {
	for s.rem > 0 && len(b) > 0 {
		b[0] ^= s.word[8-s.rem]
		b = b[1:]
		s.rem--
	}
	for len(b) >= 8 {
		v := binary.LittleEndian.Uint64(b) ^ s.next()
		binary.LittleEndian.PutUint64(b, v)
		b = b[8:]
	}
	if len(b) > 0 {
		binary.LittleEndian.PutUint64(s.word[:], s.next())
		s.rem = 8
		for i := range b {
			b[i] ^= s.word[i]
		}
		s.rem = 8 - len(b)
	}
}

// xorCopy writes src to dst XORed with the keystream, advancing the keystream
// exactly len(src) bytes. dst and src must not overlap.
func (s *obfKeystream) xorCopy(dst, src []byte) {
	for s.rem > 0 && len(src) > 0 {
		dst[0] = src[0] ^ s.word[8-s.rem]
		dst, src = dst[1:], src[1:]
		s.rem--
	}
	for len(src) >= 8 {
		v := binary.LittleEndian.Uint64(src) ^ s.next()
		binary.LittleEndian.PutUint64(dst, v)
		dst, src = dst[8:], src[8:]
	}
	if len(src) > 0 {
		binary.LittleEndian.PutUint64(s.word[:], s.next())
		s.rem = 8
		for i := range src {
			dst[i] = src[i] ^ s.word[i]
		}
		s.rem = 8 - len(src)
	}
}

var _ net.Conn = (*obfuscatedConn)(nil)
