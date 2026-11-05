// Package tunnel implements the packet data plane independently of server URI
// assembly, Kubernetes discovery and daemon RPCs.
//
// Client transports own persistent slot queues and shared network stacks. Each
// connection attempt owns one framed connection and joins its read/write pumps
// before the next attempt can consume the queue. On the server, Router's peer
// registry coordinates route membership with stack grace periods; connection
// handlers borrow those resources and join their own buffered writers on exit.
// The TUN bridge has a separate terminal lifetime from the route table.
//
// Packet buffers and reference ownership stay private to this package. Wire
// framing and gvisor construction are implemented by their respective leaf
// packages. Callers provide dial configuration and explicitly start and close
// server handlers through the service lifecycle used by endpoint.Serve.
package tunnel
