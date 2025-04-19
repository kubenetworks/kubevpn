// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package internal

import (
	"time"

	"github.com/DataDog/datadog-go/v5/statsd"
)

const DefaultDogstatsdAddr = "localhost:8125"

type StatsdClient interface {
	Incr(name string, tags []string, rate float64) error
	Count(name string, value int64, tags []string, rate float64) error
	CountWithTimestamp(name string, value int64, tags []string, rate float64, timestamp time.Time) error
	Gauge(name string, value float64, tags []string, rate float64) error
	GaugeWithTimestamp(name string, value float64, tags []string, rate float64, timestamp time.Time) error
	DistributionSamples(name string, values []float64, tags []string, rate float64) error
	Timing(name string, value time.Duration, tags []string, rate float64) error
	Flush() error
	Close() error
}

// NewStatsdClient returns a new statsd client with the provided address and globaltags
func NewStatsdClient(addr string, globalTags []string) (StatsdClient, error) {
	if addr == "" {
		addr = DefaultDogstatsdAddr
	}
	client, err := statsd.NewDirect(addr, statsd.WithMaxMessagesPerPayload(40), statsd.WithTags(globalTags))
	if err != nil {
		return &statsd.NoOpClientDirect{}, err
	}
	return client, nil
}
