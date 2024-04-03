// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"context"

	"gopkg.in/DataDog/dd-trace-go.v1/datastreams/options"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/internal"
	idatastreams "gopkg.in/DataDog/dd-trace-go.v1/internal/datastreams"
)

// dataStreamsContainer is an object that contains a data streams processor.
type dataStreamsContainer interface {
	GetDataStreamsProcessor() *idatastreams.Processor
}

// GetDataStreamsProcessor returns the processor tracking data streams stats
func (t *tracer) GetDataStreamsProcessor() *idatastreams.Processor {
	return t.dataStreams
}

// SetDataStreamsCheckpoint sets a consume or produce checkpoint in a Data Streams pathway.
// This enables tracking data flow & end to end latency.
// To learn more about the data streams product, see: https://docs.datadoghq.com/data_streams/go/
func SetDataStreamsCheckpoint(ctx context.Context, edgeTags ...string) (outCtx context.Context, ok bool) {
	return SetDataStreamsCheckpointWithParams(ctx, options.CheckpointParams{}, edgeTags...)
}

// SetDataStreamsCheckpointWithParams sets a consume or produce checkpoint in a Data Streams pathway.
// This enables tracking data flow & end to end latency.
// To learn more about the data streams product, see: https://docs.datadoghq.com/data_streams/go/
func SetDataStreamsCheckpointWithParams(ctx context.Context, params options.CheckpointParams, edgeTags ...string) (outCtx context.Context, ok bool) {
	if t, ok := internal.GetGlobalTracer().(dataStreamsContainer); ok {
		if processor := t.GetDataStreamsProcessor(); processor != nil {
			outCtx = processor.SetCheckpointWithParams(ctx, params, edgeTags...)
			return outCtx, true
		}
	}
	return ctx, false
}

// TrackKafkaCommitOffset should be used in the consumer, to track when it acks offset.
// if used together with TrackKafkaProduceOffset it can generate a Kafka lag in seconds metric.
func TrackKafkaCommitOffset(group, topic string, partition int32, offset int64) {
	if t, ok := internal.GetGlobalTracer().(dataStreamsContainer); ok {
		if p := t.GetDataStreamsProcessor(); p != nil {
			p.TrackKafkaCommitOffset(group, topic, partition, offset)
		}
	}
}

// TrackKafkaProduceOffset should be used in the producer, to track when it produces a message.
// if used together with TrackKafkaCommitOffset it can generate a Kafka lag in seconds metric.
func TrackKafkaProduceOffset(topic string, partition int32, offset int64) {
	if t, ok := internal.GetGlobalTracer().(dataStreamsContainer); ok {
		if p := t.GetDataStreamsProcessor(); p != nil {
			p.TrackKafkaProduceOffset(topic, partition, offset)
		}
	}
}

// TrackKafkaHighWatermarkOffset should be used in the producer, to track when it produces a message.
// if used together with TrackKafkaCommitOffset it can generate a Kafka lag in seconds metric.
func TrackKafkaHighWatermarkOffset(cluster string, topic string, partition int32, offset int64) {
	if t, ok := internal.GetGlobalTracer().(dataStreamsContainer); ok {
		if p := t.GetDataStreamsProcessor(); p != nil {
			p.TrackKafkaHighWatermarkOffset(cluster, topic, partition, offset)
		}
	}
}
