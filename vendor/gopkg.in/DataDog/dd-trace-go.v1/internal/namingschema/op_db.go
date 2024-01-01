// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package namingschema

import "fmt"

type dbOutboundOp struct {
	cfg    *config
	system string
}

// NewDBOutboundOp creates a new naming schema for db outbound operations.
// The V0 implementation defaults to the v1 and is meant to be overwritten if needed, since (generally) it does not
// follow any pattern among db integrations.
func NewDBOutboundOp(system string, opts ...Option) *Schema {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}
	return New(&dbOutboundOp{cfg: cfg, system: system})
}

func (d *dbOutboundOp) V0() string {
	if d.cfg.overrideV0 != nil {
		return *d.cfg.overrideV0
	}
	return d.V1()
}

func (d *dbOutboundOp) V1() string {
	return fmt.Sprintf("%s.query", d.system)
}

// NewElasticsearchOutboundOp creates a new schema for Elasticsearch (db) outbound operations.
func NewElasticsearchOutboundOp(opts ...Option) *Schema {
	return NewDBOutboundOp("elasticsearch", opts...)
}

// NewMongoDBOutboundOp creates a new schema for MongoDB (db) outbound operations.
func NewMongoDBOutboundOp(opts ...Option) *Schema {
	return NewDBOutboundOp("mongodb", opts...)
}

// NewCassandraOutboundOp creates a new schema for Cassandra (db) outbound operations.
func NewCassandraOutboundOp(opts ...Option) *Schema {
	return NewDBOutboundOp("cassandra", opts...)
}
