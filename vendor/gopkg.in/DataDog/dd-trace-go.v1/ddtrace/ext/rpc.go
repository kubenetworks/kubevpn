// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package ext

const (
	// RPCSystem identifies the RPC remoting system.
	RPCSystem = "rpc.system"
	// RPCService represents the full (logical) name of the service being called, including its package name,
	// if applicable. Note this is the logical name of the service from the RPC interface perspective,
	// which can be different from the name of any implementing class.
	RPCService = "rpc.service"
	// RPCMethod represents the name of the (logical) method being called. Note this is the logical name of the
	// method from the RPC interface perspective, which can be different from the name of
	// any implementing method/function.
	RPCMethod = "rpc.method"
)

// Well-known identifiers for rpc.system.
const (
	// RPCSystemGRPC identifies gRPC.
	RPCSystemGRPC = "grpc"
	// RPCSystemTwirp identifies Twirp.
	RPCSystemTwirp = "twirp"
)

// gRPC specific tags.
const (
	// GRPCFullMethod represents the full name of the logical method being called following the
	// format: /$package.$service/$method
	GRPCFullMethod = "rpc.grpc.full_method"
)
