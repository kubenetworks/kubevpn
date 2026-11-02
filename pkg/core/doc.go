// Package core assembles protocol servers and outbound connections from Node URIs.
// It owns TCP/TLS dialing, connectors, SSH listeners, UDP relay/reverse bridge and
// Serve's listener/accepted-handler lifecycle. Packet routing, connection slots,
// TUN devices, heartbeats and gvisor peer resources belong to package tunnel.
// NewRouteHub returns tunnel's actual Router; Forwarder passes a dial function
// across that boundary without coupling tunnel to core assembly.
// DNS configuration and sidecar injection do not belong here.
package core
