// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package internal

import (
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

type StatsdClient interface {
	Incr(name string, tags []string, rate float64) error
	Count(name string, value int64, tags []string, rate float64) error
	Gauge(name string, value float64, tags []string, rate float64) error
	Timing(name string, value time.Duration, tags []string, rate float64) error
	Flush() error
	Close() error
}

type Stat interface {
	Name() string
	Value() interface{}
	Tags() []string
	Rate() float64
}

// Gauge implements the Stat interface and contains a float64 value
type Gauge struct {
	name  string
	value float64
	tags  []string
	rate  float64
}

func NewGauge(name string, value float64, tags []string, rate float64) Gauge {
	return Gauge{
		name:  name,
		value: value,
		tags:  tags,
		rate:  rate,
	}
}

func (g Gauge) Name() string {
	return g.name
}

func (g Gauge) Value() interface{} {
	return g.value
}

func (g Gauge) Tags() []string {
	return g.tags
}

func (g Gauge) Rate() float64 {
	return g.rate
}

// Count implements the Stat interface and contains a int64 value
type Count struct {
	name  string
	value int64
	tags  []string
	rate  float64
}

func NewCount(name string, value int64, tags []string, rate float64) Count {
	return Count{
		name:  name,
		value: value,
		tags:  tags,
		rate:  rate,
	}
}

func (c Count) Name() string {
	return c.name
}

func (c Count) Value() interface{} {
	return c.value
}

func (c Count) Tags() []string {
	return c.tags
}

func (c Count) Rate() float64 {
	return c.rate
}

// Timing implements the Stat interface and contains a time.Duration value
type Timing struct {
	name  string
	value time.Duration
	tags  []string
	rate  float64
}

func NewTiming(name string, value time.Duration, tags []string, rate float64) Timing {
	return Timing{
		name:  name,
		value: value,
		tags:  tags,
		rate:  rate,
	}
}

func (t Timing) Name() string {
	return t.name
}

func (t Timing) Value() interface{} {
	return t.value
}

func (t Timing) Tags() []string {
	return t.tags
}

func (t Timing) Rate() float64 {
	return t.rate
}

// StatsCarrier collects stats on its contribStats channel and submits them through a statsd client
type StatsCarrier struct {
	contribStats chan Stat
	statsd       StatsdClient
	stop         chan struct{}
	wg           sync.WaitGroup
	stopped      uint64
}

func NewStatsCarrier(statsd StatsdClient) *StatsCarrier {
	return &StatsCarrier{
		contribStats: make(chan Stat),
		statsd:       statsd,
		stopped:      1,
	}
}

// Start runs the StatsCarrier in a goroutine
// The caller of sc.Start() is resopnsible for calling sc.Stop()
func (sc *StatsCarrier) Start() {
	if atomic.SwapUint64(&sc.stopped, 0) == 0 {
		// already running
		log.Warn("(*StatsCarrier).Start called more than once. This is likely a programming error.")
		return
	}
	sc.stop = make(chan struct{})
	sc.wg.Add(1)
	go func() {
		defer sc.wg.Done()
		sc.run()
	}()
}

func (sc *StatsCarrier) run() {
	for {
		select {
		case stat := <-sc.contribStats:
			sc.push(stat)
		case <-sc.stop:
			// make sure to flush any stats still in the channel
			if len(sc.contribStats) > 0 {
				sc.push(<-sc.contribStats)
			}
			return
		}
	}
}

// Stop closes the StatsCarrier's stop channel
func (sc *StatsCarrier) Stop() {
	if atomic.SwapUint64(&(sc.stopped), 1) > 0 {
		return
	}
	close(sc.stop)
	sc.wg.Wait()
}

// push submits the stat of supported types (gauge, count or timing) via its statsd client
func (sc *StatsCarrier) push(s Stat) {
	switch s.(type) {
	case Gauge:
		v, ok := s.Value().(float64)
		if !ok {
			log.Debug("Received gauge stat with incompatible value; looking for float64 value but got %T. Dropping stat %v.", s.Value(), s.Name())
			break
		}
		sc.statsd.Gauge(s.Name(), v, s.Tags(), s.Rate())
	case Count:
		v, ok := s.Value().(int64)
		if !ok {
			log.Debug("Received count stat with incompatible value; looking for int64 value but got %T. Dropping stat %v.", s.Value(), s.Name())
			break
		}
		sc.statsd.Count(s.Name(), v, s.Tags(), s.Rate())
	case Timing:
		v, ok := s.Value().(time.Duration)
		if !ok {
			log.Debug("Received timing stat with incompatible value; looking for time.Duration value but got %T. Dropping stat %v.", s.Value(), s.Name())
			break
		}
		sc.statsd.Timing(s.Name(), v, s.Tags(), s.Rate())
	default:
		log.Debug("Stat submission failed: metric type for %v not supported", s.Name())
	}
}

// Add pushes the Stat, s, onto the StatsCarrier's contribStats channel
func (sc *StatsCarrier) Add(s Stat) {
	sc.contribStats <- s
}
