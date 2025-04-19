// Package runtimemetrics exports all runtime/metrics via statsd on a regular interval.
package runtimemetrics

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"runtime/metrics"
	"strings"
	"sync"
	"time"
)

// pollFrequency is the frequency at which we poll runtime/metrics and report
// them to statsd. The statsd client aggregates this data, usually over a 2s
// window [1], and so does the agent, usually over a 10s window [2].
//
// Our goal is to submit one data point per aggregation window, using the
// CountWithTimestamp / GaugeWithTimestamp APIs for submitting precisely aligned
// metrics, to enable comparing them with one another.
//
// [1] https://github.com/DataDog/datadog-go/blob/e612112c8bb396b33ad5d9edd645d289b07d0e40/statsd/options.go/#L23
// [2] https://docs.datadoghq.com/developers/dogstatsd/data_aggregation/#how-is-aggregation-performed-with-the-dogstatsd-server
var pollFrequency = 10 * time.Second

var unknownMetricLogOnce, unsupportedKindLogOnce sync.Once

// mu protects the variables below
var mu sync.Mutex
var enabled bool

// NOTE: The Start method below is intentionally minimal for now. We probably want to think about
// this API a bit more before we publish it in dd-trace-go. I.e. do we want to make the
// pollFrequency configurable (higher resolution at the cost of higher overhead on the agent and
// statsd library)? Do we want to support multiple instances? We probably also want a (flushing?)
// stop method.

// Start starts reporting runtime/metrics to the given statsd client.
func Start(statsd partialStatsdClientInterface, logger *slog.Logger) error {
	mu.Lock()
	defer mu.Unlock()

	if enabled {
		// We could support multiple instances, but the use cases for it are not
		// clear, so for now let's consider this to be a misconfiguration.
		return errors.New("runtimemetrics has already been started")
	}

	descs := metrics.All()
	rms := newRuntimeMetricStore(descs, statsd, logger)
	// TODO: Go services experiencing high scheduling latency might see a
	// large variance for the period in between rms.report calls. This might
	// cause spikes in cumulative metric reporting. Should we try to correct
	// for this by measuring the actual reporting time delta and
	// extrapolating our numbers?
	//
	// Another challenge is that some metrics only update after GC mark
	// termination, see [1][2]. This means that it's likely that the rate of
	// submission for those metrics will be dependant on the service's workload
	// and GC configuration.
	//
	// [1] https://github.com/golang/go/blob/go1.21.3/src/runtime/mstats.go#L939
	// [2] https://github.com/golang/go/issues/59749
	go func() {
		for range time.Tick(pollFrequency) {
			rms.report()
		}
	}()
	enabled = true
	return nil
}

type runtimeMetric struct {
	ddMetricName string
	cumulative   bool

	currentValue  metrics.Value
	previousValue metrics.Value
	timestamp     time.Time
}

// the map key is the name of the metric in runtime/metrics
type runtimeMetricStore struct {
	metrics  map[string]*runtimeMetric
	statsd   partialStatsdClientInterface
	logger   *slog.Logger
	baseTags []string
}

// partialStatsdClientInterface is the subset of statsd.ClientInterface that is
// used by this package.
type partialStatsdClientInterface interface {
	// Rate is used in the datadog-go statsd library to sample to values sent,
	// we should always submit a rate >=1 to ensure our submissions are not sampled.
	// The rate is forwarded to the agent but then discarded for gauge metrics.
	GaugeWithTimestamp(name string, value float64, tags []string, rate float64, timestamp time.Time) error
	CountWithTimestamp(name string, value int64, tags []string, rate float64, timestamp time.Time) error
	DistributionSamples(name string, values []float64, tags []string, rate float64) error
}

func newRuntimeMetricStore(descs []metrics.Description, statsdClient partialStatsdClientInterface, logger *slog.Logger) runtimeMetricStore {
	rms := runtimeMetricStore{
		metrics:  map[string]*runtimeMetric{},
		statsd:   statsdClient,
		logger:   logger,
		baseTags: getBaseTags(),
	}

	for _, d := range descs {
		cumulative := d.Cumulative

		// /sched/latencies:seconds is incorrectly set as non-cumulative,
		// fixed by https://go-review.googlesource.com/c/go/+/486755
		// TODO: Use a build tag to apply this logic to Go versions < 1.20.
		if d.Name == "/sched/latencies:seconds" {
			cumulative = true
		}

		ddMetricName, err := datadogMetricName(d.Name)
		if err != nil {
			rms.logger.Warn("runtimemetrics: not reporting one of the runtime metrics", slog.Attr{Key: "error", Value: slog.StringValue(err.Error())})
			continue
		}

		rms.metrics[d.Name] = &runtimeMetric{
			ddMetricName: ddMetricName,
			cumulative:   cumulative,
		}
	}

	rms.update()

	return rms
}

func (rms runtimeMetricStore) update() {
	// TODO: Reuse this slice to avoid allocations? Note: I don't see these
	// allocs show up in profiling.
	samples := make([]metrics.Sample, len(rms.metrics))
	i := 0
	// NOTE: Map iteration in Go is randomized, so we end up randomizing the
	// samples slice. In theory this should not impact correctness, but it's
	// worth keeping in mind in case problems are observed in the future.
	for name := range rms.metrics {
		samples[i].Name = name
		i++
	}
	metrics.Read(samples)
	timestamp := time.Now()
	for _, s := range samples {
		runtimeMetric := rms.metrics[s.Name]

		runtimeMetric.previousValue = runtimeMetric.currentValue
		runtimeMetric.currentValue = s.Value
		runtimeMetric.timestamp = timestamp
	}
}

func (rms runtimeMetricStore) report() {
	rms.update()
	samples := []distributionSample{}

	for name, rm := range rms.metrics {
		switch rm.currentValue.Kind() {
		case metrics.KindUint64:
			v := rm.currentValue.Uint64()
			// if the value didn't change between two reporting
			// cycles, don't submit anything. this avoids having
			// inaccurate drops to zero
			// we submit 0 values to be able to distinguish between
			// cases where the metric was never reported as opposed
			// to the metric always being equal to zero
			if rm.cumulative && v != 0 && v == rm.previousValue.Uint64() {
				continue
			}

			// Some of the Uint64 metrics are actually calculated as a difference by the Go runtime: v = uint64(x - y)
			//
			// Notably, this means that if x < y, then v will be roughly MaxUint64 (minus epsilon).
			// This then shows up as '16 EiB' in Datadog graphs, because MaxUint64 bytes = 2^64 = 2^(4 + 10x6) = 2^4 x (2^10)^6 = 16 x 1024^6 = 16 EiB.
			//
			// This is known to happen with the '/memory/classes/heap/unused:bytes' metric: https://github.com/golang/go/blob/go1.22.1/src/runtime/metrics.go#L364
			// Until this bug is fixed, we log the problematic value and skip submitting that point to avoid spurious spikes in graphs.
			if v > math.MaxUint64/2 {
				tags := make([]string, 0, len(rms.baseTags)+1)
				tags = append(tags, rms.baseTags...)
				tags = append(tags, "metric_name:"+rm.ddMetricName)
				rms.statsd.CountWithTimestamp("runtime.go.metrics.skipped_values", 1, tags, 1, rm.timestamp)

				// Some metrics are ~sort of expected to report this high value (e.g.
				// "runtime.go.metrics.gc_gogc.percent" will consistently report "MaxUint64 - 1" if
				// GOGC is OFF). We only want to log the full heap stats for the not-so-expected
				// case of "heap unused bytes".
				if name == "/memory/classes/heap/unused:bytes" {
					logAttrs := []any{
						slog.Attr{Key: "metric_name", Value: slog.StringValue(rm.ddMetricName)},
						slog.Attr{Key: "timestamp", Value: slog.TimeValue(rm.timestamp)},
						slog.Attr{Key: "uint64(x-y)", Value: slog.Uint64Value(v)},
						slog.Attr{
							// If v is very close to MaxUint64, it will be hard to read "how negative was x-y", so we compute it here for convenience:
							Key:   "int64(x-y)",
							Value: slog.Int64Value(-int64(math.MaxUint64 - v + 1)), // the '+1' is necessary because if int64(x-y)=-1, then uint64(x-y)=MaxUint64
						},
					}

					// Append all Uint64 values for maximum observability
					for name, rm := range rms.metrics {
						if rm.currentValue.Kind() == metrics.KindUint64 {
							logAttrs = append(logAttrs, slog.Attr{Key: name, Value: slog.Uint64Value(rm.currentValue.Uint64())})
						}
					}

					rms.logger.Warn("runtimemetrics: skipped submission of absurd value", logAttrs...)
				}
				continue
			}

			rms.statsd.GaugeWithTimestamp(rm.ddMetricName, float64(v), rms.baseTags, 1, rm.timestamp)
		case metrics.KindFloat64:
			v := rm.currentValue.Float64()
			// if the value didn't change between two reporting
			// cycles, don't submit anything. this avoids having
			// inaccurate drops to zero
			// we submit 0 values to be able to distinguish between
			// cases where the metric was never reported as opposed
			// to the metric always being equal to zero
			if rm.cumulative && v != 0 && v == rm.previousValue.Float64() {
				continue
			}
			rms.statsd.GaugeWithTimestamp(rm.ddMetricName, v, rms.baseTags, 1, rm.timestamp)
		case metrics.KindFloat64Histogram:
			v := rm.currentValue.Float64Histogram()
			var equal bool
			if rm.cumulative {
				// Note: This branch should ALWAYS be taken as of go1.21.
				v, equal = sub(v, rm.previousValue.Float64Histogram())
				// if the histogram didn't change between two reporting
				// cycles, don't submit anything. this avoids having
				// inaccurate drops to zero for percentile metrics
				if equal {
					continue
				}
			}

			samples = samples[:0]
			distSamples := distributionSamplesFromHist(v, samples)
			values := make([]float64, len(distSamples))
			for i, ds := range distSamples {
				values[i] = ds.Value
				rms.statsd.DistributionSamples(rm.ddMetricName, values[i:i+1], rms.baseTags, ds.Rate)
			}

			stats := statsFromHist(v)
			// TODO: Could/should we use datadog distribution metrics for this?
			rms.statsd.GaugeWithTimestamp(rm.ddMetricName+".avg", stats.Avg, rms.baseTags, 1, rm.timestamp)
			rms.statsd.GaugeWithTimestamp(rm.ddMetricName+".min", stats.Min, rms.baseTags, 1, rm.timestamp)
			rms.statsd.GaugeWithTimestamp(rm.ddMetricName+".max", stats.Max, rms.baseTags, 1, rm.timestamp)
			rms.statsd.GaugeWithTimestamp(rm.ddMetricName+".median", stats.Median, rms.baseTags, 1, rm.timestamp)
			rms.statsd.GaugeWithTimestamp(rm.ddMetricName+".p95", stats.P95, rms.baseTags, 1, rm.timestamp)
			rms.statsd.GaugeWithTimestamp(rm.ddMetricName+".p99", stats.P99, rms.baseTags, 1, rm.timestamp)
		case metrics.KindBad:
			// This should never happen because all metrics are supported
			// by construction.
			unknownMetricLogOnce.Do(func() {
				rms.logger.Error("runtimemetrics: encountered an unknown metric, this should never happen and might indicate a bug", slog.Attr{Key: "metric_name", Value: slog.StringValue(name)})
			})
		default:
			// This may happen as new metric kinds get added.
			//
			// The safest thing to do here is to simply log it somewhere once
			// as something to look into, but ignore it for now.
			unsupportedKindLogOnce.Do(func() {
				rms.logger.Error("runtimemetrics: unsupported metric kind, support for that kind should be added in pkg/runtimemetrics",
					slog.Attr{Key: "metric_name", Value: slog.StringValue(name)},
					slog.Attr{Key: "kind", Value: slog.AnyValue(rm.currentValue.Kind())},
				)
			})
		}
	}
}

// regex extracted from https://cs.opensource.google/go/go/+/refs/tags/go1.20.3:src/runtime/metrics/description.go;l=13
var runtimeMetricRegex = regexp.MustCompile("^(?P<name>/[^:]+):(?P<unit>[^:*/]+(?:[*/][^:*/]+)*)$")

// see https://docs.datadoghq.com/metrics/custom_metrics/#naming-custom-metrics
var datadogMetricRegex = regexp.MustCompile(`[^a-zA-Z0-9\._]`)

func datadogMetricName(runtimeName string) (string, error) {
	m := runtimeMetricRegex.FindStringSubmatch(runtimeName)

	if len(m) != 3 {
		return "", fmt.Errorf("failed to parse metric name for metric %s", runtimeName)
	}

	// strip leading "/"
	metricPath := strings.TrimPrefix(m[1], "/")
	metricUnit := m[2]

	name := datadogMetricRegex.ReplaceAllString(metricPath+"."+metricUnit, "_")

	// Note: This prefix is special. Don't change it without consulting the
	// runtime/metrics squad.
	return "runtime.go.metrics." + name, nil
}
