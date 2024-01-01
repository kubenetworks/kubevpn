// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package namingschema

import "fmt"

type clientOutboundOp struct {
	cfg    *config
	system string
}

// NewClientOutboundOp creates a new naming schema for client outbound operations.
func NewClientOutboundOp(system string, opts ...Option) *Schema {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}
	return New(&clientOutboundOp{cfg: cfg, system: system})
}

func (c *clientOutboundOp) V0() string {
	if c.cfg.overrideV0 != nil {
		return *c.cfg.overrideV0
	}
	return fmt.Sprintf("%s.request", c.system)
}

func (c *clientOutboundOp) V1() string {
	return fmt.Sprintf("%s.client.request", c.system)
}

type serverInboundOp struct {
	cfg    *config
	system string
}

// NewServerInboundOp creates a new naming schema for server inbound operations.
func NewServerInboundOp(system string, opts ...Option) *Schema {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}
	return New(&serverInboundOp{cfg: cfg, system: system})
}

func (s *serverInboundOp) V0() string {
	if s.cfg.overrideV0 != nil {
		return *s.cfg.overrideV0
	}
	return fmt.Sprintf("%s.request", s.system)
}

func (s *serverInboundOp) V1() string {
	return fmt.Sprintf("%s.server.request", s.system)
}

// NewHTTPClientOp creates a new schema for HTTP client outbound operations.
func NewHTTPClientOp(opts ...Option) *Schema {
	return NewClientOutboundOp("http", opts...)
}

// NewHTTPServerOp creates a new schema for HTTP server inbound operations.
func NewHTTPServerOp(opts ...Option) *Schema {
	return NewServerInboundOp("http", opts...)
}

// NewGRPCClientOp creates a new schema for gRPC client outbound operations.
func NewGRPCClientOp(opts ...Option) *Schema {
	newOpts := append([]Option{WithOverrideV0("grpc.client")}, opts...)
	return NewClientOutboundOp("grpc", newOpts...)
}

// NewGRPCServerOp creates a new schema for gRPC server inbound operations.
func NewGRPCServerOp(opts ...Option) *Schema {
	newOpts := append([]Option{WithOverrideV0("grpc.server")}, opts...)
	return NewServerInboundOp("grpc", newOpts...)
}

// NewGraphqlServerOp creates a new schema for GraphQL server inbound operations.
func NewGraphqlServerOp(opts ...Option) *Schema {
	return NewServerInboundOp("graphql", opts...)
}
