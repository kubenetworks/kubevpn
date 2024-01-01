// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:generate msgp -unexported -marshal=false -o=payload_msgp.go -tests=false

package datastreams

// StatsPayload stores client computed stats.
type StatsPayload struct {
	// Env specifies the env. of the application, as defined by the user.
	Env string
	// Service is the service of the application
	Service string
	// Stats holds all stats buckets computed within this payload.
	Stats []StatsBucket
	// TracerVersion is the version of the tracer
	TracerVersion string
	// Lang is the language of the tracer
	Lang string
	// Version is the version of the service
	Version string
}

type ProduceOffset struct {
	Topic     string
	Partition int32
	Offset    int64
}

type CommitOffset struct {
	ConsumerGroup string
	Topic         string
	Partition     int32
	Offset        int64
}

// Backlog represents the size of a queue that hasn't been yet read by the consumer.
type Backlog struct {
	// Tags that identify the backlog
	Tags []string
	// Value of the backlog
	Value int64
}

// StatsBucket specifies a set of stats computed over a duration.
type StatsBucket struct {
	// Start specifies the beginning of this bucket in unix nanoseconds.
	Start uint64
	// Duration specifies the duration of this bucket in nanoseconds.
	Duration uint64
	// Stats contains a set of statistics computed for the duration of this bucket.
	Stats []StatsPoint
	// Backlogs store information used to compute queue backlog
	Backlogs []Backlog
}

// TimestampType can be either current or origin.
type TimestampType string

const (
	// TimestampTypeCurrent is for when the recorded timestamp is based on the
	// timestamp of the current StatsPoint.
	TimestampTypeCurrent TimestampType = "current"
	// TimestampTypeOrigin is for when the recorded timestamp is based on the
	// time that the first StatsPoint in the pathway is sent out.
	TimestampTypeOrigin TimestampType = "origin"
)

// StatsPoint contains a set of statistics grouped under various aggregation keys.
type StatsPoint struct {
	// These fields indicate the properties under which the stats were aggregated.
	Service    string // deprecated
	EdgeTags   []string
	Hash       uint64
	ParentHash uint64
	// These fields specify the stats for the above aggregation.
	// those are distributions of latency in seconds.
	PathwayLatency []byte
	EdgeLatency    []byte
	PayloadSize    []byte
	TimestampType  TimestampType
}
