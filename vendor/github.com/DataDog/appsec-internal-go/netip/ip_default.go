// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

//go:build !go1.19
// +build !go1.19

package netip

import "inet.af/netaddr"

// Addr wraps an netaddr.IP value
type Addr = netaddr.IP

// Prefix wraps an netaddr.IPPrefix value
type Prefix = netaddr.IPPrefix

var (
	// ParseAddr wraps the netaddr.ParseIP function
	ParseAddr = netaddr.ParseIP
	// ParsePrefix wraps the netaddr.ParseIPPrefix function
	ParsePrefix = netaddr.ParseIPPrefix
	// MustParsePrefix wraps the netaddr.MustParseIPPrefix function
	MustParsePrefix = netaddr.MustParseIPPrefix
	// MustParseAddr wraps the netaddr.MustParseIP function
	MustParseAddr = netaddr.MustParseIP
	// IPv4 wraps the netaddr.IPv4 function
	IPv4 = netaddr.IPv4
	// AddrFrom16 wraps the netaddr.IPv6Raw function
	AddrFrom16 = netaddr.IPv6Raw
)
