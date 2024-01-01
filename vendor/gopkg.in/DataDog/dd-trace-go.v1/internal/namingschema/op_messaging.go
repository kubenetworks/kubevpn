// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package namingschema

import "fmt"

type messagingOutboundOp struct {
	cfg    *config
	system string
}

// NewMessagingOutboundOp creates a new naming schema for outbound operations from messaging systems.
func NewMessagingOutboundOp(system string, opts ...Option) *Schema {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}
	return New(&messagingOutboundOp{cfg: cfg, system: system})
}

func (m *messagingOutboundOp) V0() string {
	if m.cfg.overrideV0 != nil {
		return *m.cfg.overrideV0
	}
	return m.V1()
}

func (m *messagingOutboundOp) V1() string {
	return fmt.Sprintf("%s.send", m.system)
}

type messagingInboundOp struct {
	cfg    *config
	system string
}

// NewMessagingInboundOp creates a new schema for messaging systems inbound operations.
// The V0 implementation defaults to the v1 and is meant to be overwritten if needed, since (generally) it does not
// follow any pattern among messaging integrations.
func NewMessagingInboundOp(system string, opts ...Option) *Schema {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}
	return New(&messagingInboundOp{cfg: cfg, system: system})
}

func (m *messagingInboundOp) V0() string {
	if m.cfg.overrideV0 != nil {
		return *m.cfg.overrideV0
	}
	return m.V1()
}

func (m *messagingInboundOp) V1() string {
	return fmt.Sprintf("%s.process", m.system)
}

// NewKafkaOutboundOp creates a new schema for Kafka (messaging) outbound operations.
func NewKafkaOutboundOp(opts ...Option) *Schema {
	newOpts := append([]Option{WithOverrideV0("kafka.produce")}, opts...)
	return NewMessagingOutboundOp("kafka", newOpts...)
}

// NewKafkaInboundOp creates a new schema for Kafka (messaging) inbound operations.
func NewKafkaInboundOp(opts ...Option) *Schema {
	newOpts := append([]Option{WithOverrideV0("kafka.consume")}, opts...)
	return NewMessagingInboundOp("kafka", newOpts...)
}

// NewGCPPubsubInboundOp creates a new schema for GCP Pubsub (messaging) inbound operations.
func NewGCPPubsubInboundOp(opts ...Option) *Schema {
	newOpts := append([]Option{WithOverrideV0("pubsub.receive")}, opts...)
	return NewMessagingInboundOp("gcp.pubsub", newOpts...)
}

// NewGCPPubsubOutboundOp creates a new schema for GCP Pubsub (messaging) outbound operations.
func NewGCPPubsubOutboundOp(opts ...Option) *Schema {
	newOpts := append([]Option{WithOverrideV0("pubsub.publish")}, opts...)
	return NewMessagingOutboundOp("gcp.pubsub", newOpts...)
}
