// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

package netip

import "net/netip"

// Addr wraps a netip.Addr value
type Addr = netip.Addr

// Prefix wraps a netip.Prefix value
type Prefix = netip.Prefix

var (
	// ParseAddr wraps the netip.ParseAddr function
	ParseAddr = netip.ParseAddr
	// MustParsePrefix wraps the netip.MustParsePrefix function
	MustParsePrefix = netip.MustParsePrefix
	// MustParseAddr wraps the netip.MustParseAddr function
	MustParseAddr = netip.MustParseAddr
	// AddrFrom16 wraps the netIP.AddrFrom16 function
	AddrFrom16 = netip.AddrFrom16
)

// IPv4 wraps the netip.AddrFrom4 function
func IPv4(a, b, c, d byte) Addr {
	e := [4]byte{a, b, c, d}
	return netip.AddrFrom4(e)
}
