// Package wire provides the length-prefixed datagram framing shared by tunnel
// data connections and protocol relays. It owns stream framing, not packet
// routing, queues or application sessions.
//
// Short writes continue only on the same connection. A damaged stream is
// closed and partial frames are never replayed on another connection. Batch
// writers report actual byte progress so callers can account for the complete
// frame prefix without changing packet ownership.
package wire
