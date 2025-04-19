// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/DataDog/datadog-agent/pkg/obfuscate"
	"github.com/DataDog/datadog-agent/pkg/trace/stats"
	"gopkg.in/DataDog/dd-trace-go.v1/internal"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/civisibility/constants"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/civisibility/utils"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"

	"github.com/DataDog/datadog-go/v5/statsd"
)

// defaultStatsBucketSize specifies the default span of time that will be
// covered in one stats bucket.
var defaultStatsBucketSize = (10 * time.Second).Nanoseconds()

// concentrator aggregates and stores statistics on incoming spans in time buckets,
// flushing them occasionally to the underlying transport located in the given
// tracer config.
type concentrator struct {
	// In specifies the channel to be used for feeding data to the concentrator.
	// In order for In to have a consumer, the concentrator must be started using
	// a call to Start.
	In chan *tracerStatSpan

	// stopped reports whether the concentrator is stopped (when non-zero)
	stopped uint32

	spanConcentrator *stats.SpanConcentrator

	aggregationKey stats.PayloadAggregationKey

	wg           sync.WaitGroup        // waits for any active goroutines
	bucketSize   int64                 // the size of a bucket in nanoseconds
	stop         chan struct{}         // closing this channel triggers shutdown
	cfg          *config               // tracer startup configuration
	statsdClient internal.StatsdClient // statsd client for sending metrics.
}

type tracerStatSpan struct {
	statSpan *stats.StatSpan
	origin   string
}

// newConcentrator creates a new concentrator using the given tracer
// configuration c. It creates buckets of bucketSize nanoseconds duration.
func newConcentrator(c *config, bucketSize int64, statsdClient internal.StatsdClient) *concentrator {
	sCfg := &stats.SpanConcentratorConfig{
		ComputeStatsBySpanKind: false,
		BucketInterval:         defaultStatsBucketSize,
	}
	env := c.agent.defaultEnv
	if c.env != "" {
		env = c.env
	}
	if env == "" {
		// We do this to avoid a panic in the stats calculation logic when env is empty
		// This should never actually happen as the agent MUST have an env configured to start-up
		// That panic will be removed in a future release at which point we can remove this
		env = "unknown-env"
		log.Debug("No DD Env found, normally the agent should have one")
	}
	gitCommitSha := ""
	if c.ciVisibilityEnabled {
		// We only have this data if we're in CI Visibility
		gitCommitSha = utils.GetCITags()[constants.GitCommitSHA]
	}
	aggKey := stats.PayloadAggregationKey{
		Hostname:     c.hostname,
		Env:          env,
		Version:      c.version,
		ContainerID:  "", // This intentionally left empty as the Agent will attach the container ID only in certain situations.
		GitCommitSha: gitCommitSha,
		ImageTag:     "",
	}
	spanConcentrator := stats.NewSpanConcentrator(sCfg, time.Now())
	return &concentrator{
		In:               make(chan *tracerStatSpan, 10000),
		bucketSize:       bucketSize,
		stopped:          1,
		cfg:              c,
		aggregationKey:   aggKey,
		spanConcentrator: spanConcentrator,
		statsdClient:     statsdClient,
	}
}

// alignTs returns the provided timestamp truncated to the bucket size.
// It gives us the start time of the time bucket in which such timestamp falls.
func alignTs(ts, bucketSize int64) int64 { return ts - ts%bucketSize }

// Start starts the concentrator. A started concentrator needs to be stopped
// in order to gracefully shut down, using Stop.
func (c *concentrator) Start() {
	if atomic.SwapUint32(&c.stopped, 0) == 0 {
		// already running
		log.Warn("(*concentrator).Start called more than once. This is likely a programming error.")
		return
	}
	c.stop = make(chan struct{})
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		tick := time.NewTicker(time.Duration(c.bucketSize) * time.Nanosecond)
		defer tick.Stop()
		c.runFlusher(tick.C)
	}()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.runIngester()
	}()
}

// runFlusher runs the flushing loop which sends stats to the underlying transport.
func (c *concentrator) runFlusher(tick <-chan time.Time) {
	for {
		select {
		case now := <-tick:
			c.flushAndSend(now, withoutCurrentBucket)
		case <-c.stop:
			return
		}
	}
}

// statsd returns any tracer configured statsd client, or a no-op.
func (c *concentrator) statsd() internal.StatsdClient {
	if c.statsdClient == nil {
		return &statsd.NoOpClientDirect{}
	}
	return c.statsdClient
}

// runIngester runs the loop which accepts incoming data on the concentrator's In
// channel.
func (c *concentrator) runIngester() {
	for {
		select {
		case s := <-c.In:
			c.statsd().Incr("datadog.tracer.stats.spans_in", nil, 1)
			c.add(s)
		case <-c.stop:
			return
		}
	}
}

func (c *concentrator) newTracerStatSpan(s *span, obfuscator *obfuscate.Obfuscator) (*tracerStatSpan, bool) {
	statSpan, ok := c.spanConcentrator.NewStatSpan(s.Service, obfuscatedResource(obfuscator, s.Type, s.Resource),
		s.Name, s.Type, s.ParentID, s.Start, s.Duration, s.Error, s.Meta, s.Metrics, c.cfg.agent.peerTags)
	if !ok {
		return nil, false
	}
	origin := s.Meta[keyOrigin]
	return &tracerStatSpan{
		statSpan: statSpan,
		origin:   origin,
	}, true
}

// add s into the concentrator's internal stats buckets.
func (c *concentrator) add(s *tracerStatSpan) {
	c.spanConcentrator.AddSpan(s.statSpan, c.aggregationKey, "", nil, s.origin)
}

// Stop stops the concentrator and blocks until the operation completes.
func (c *concentrator) Stop() {
	if atomic.SwapUint32(&c.stopped, 1) > 0 {
		return
	}
	close(c.stop)
	c.wg.Wait()
drain:
	for {
		select {
		case s := <-c.In:
			c.statsd().Incr("datadog.tracer.stats.spans_in", nil, 1)
			c.add(s)
		default:
			break drain
		}
	}
	c.flushAndSend(time.Now(), withCurrentBucket)
}

const (
	withCurrentBucket    = true
	withoutCurrentBucket = false
)

// flushAndSend flushes all the stats buckets with the given timestamp and sends them using the transport specified in
// the concentrator config. The current bucket is only included if includeCurrent is true, such as during shutdown.
func (c *concentrator) flushAndSend(timenow time.Time, includeCurrent bool) {
	csps := c.spanConcentrator.Flush(timenow.UnixNano(), includeCurrent)

	if len(csps) == 0 {
		// nothing to flush
		return
	}
	c.statsd().Incr("datadog.tracer.stats.flush_payloads", nil, float64(len(csps)))
	flushedBuckets := 0
	// Given we use a constant PayloadAggregationKey there should only ever be 1 of these, but to be forward
	// compatible in case this ever changes we can just iterate through all of them.
	for _, csp := range csps {
		flushedBuckets += len(csp.Stats)
		if err := c.cfg.transport.sendStats(csp); err != nil {
			c.statsd().Incr("datadog.tracer.stats.flush_errors", nil, 1)
			log.Error("Error sending stats payload: %v", err)
		}
	}
	c.statsd().Incr("datadog.tracer.stats.flush_buckets", nil, float64(flushedBuckets))
}
