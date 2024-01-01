// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package namingschema

import "fmt"

type cacheOutboundOp struct {
	cfg    *config
	system string
}

// NewCacheOutboundOp creates a new naming schema for outbound operations from caching systems.
// The V0 implementation defaults to the v1 and is meant to be overwritten if needed, since (generally) it does not
// follow any pattern among cache integrations.
func NewCacheOutboundOp(system string, opts ...Option) *Schema {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}
	return New(&cacheOutboundOp{cfg: cfg, system: system})
}

func (c *cacheOutboundOp) V0() string {
	if c.cfg.overrideV0 != nil {
		return *c.cfg.overrideV0
	}
	return c.V1()
}

func (c *cacheOutboundOp) V1() string {
	return fmt.Sprintf("%s.command", c.system)
}

// NewMemcachedOutboundOp creates a new schema for Memcached (cache) outbound operations.
func NewMemcachedOutboundOp(opts ...Option) *Schema {
	newOpts := append([]Option{WithOverrideV0("memcached.query")}, opts...)
	return NewCacheOutboundOp("memcached", newOpts...)
}

// NewRedisOutboundOp creates a new schema for Redis (cache) outbound operations.
func NewRedisOutboundOp(opts ...Option) *Schema {
	return NewCacheOutboundOp("redis", opts...)
}
