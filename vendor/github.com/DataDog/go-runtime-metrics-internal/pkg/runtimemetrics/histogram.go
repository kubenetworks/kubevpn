package runtimemetrics

import (
	"math"
	"runtime/metrics"
	"slices"
	"sort"
)

// As of 2023/04, the statsd client does not support sending fully formed
// histograms to the datadog-agent.
//
// These helpers extract the histograms exported by the runtime/metrics
// package into multiple values representing: avg, min, max, median, p95
// and p99 values of these histograms, so we can submit them as gauges to
// the agent.

type histogramStats struct {
	Avg    float64
	Min    float64 // aka P0
	Median float64 // aka P50
	P95    float64
	P99    float64
	Max    float64 // aka P100
}

type distributionSample struct {
	Value float64
	Rate  float64
}

func distributionSamplesFromHist(h *metrics.Float64Histogram, samples []distributionSample) []distributionSample {
	for i, count := range h.Counts {
		start, end := h.Buckets[i], h.Buckets[i+1]
		// Handle edge cases where start or end of buckets could be infinity
		if i == 0 && math.IsInf(h.Buckets[0], -1) {
			start = end
		}
		if i == len(h.Counts)-1 && math.IsInf(h.Buckets[len(h.Buckets)-1], 1) {
			end = start
		}
		if start == end && math.IsInf(start, 0) {
			// All buckets are empty, return early
			return samples
		}

		if count == 0 {
			// Don't submit empty buckets
			continue
		}

		sample := distributionSample{
			Value: (start + end) / 2,
			Rate:  1 / float64(count),
		}
		samples = append(samples, sample)
	}
	return samples
}

func statsFromHist(h *metrics.Float64Histogram) *histogramStats {
	p := percentiles(h, []float64{0, 0.5, 0.95, 0.99, 1})
	return &histogramStats{
		Avg:    avg(h),
		Min:    p[0],
		Median: p[1],
		P95:    p[2],
		P99:    p[3],
		Max:    p[4],
	}
}

// Return the difference between both histograms, and whether
// the two histograms are equal
// We assume a and b always have the same lengths for `Counts` and
// `Buckets` slices which is guaranteed by the runtime/metrics
// package: https://go.dev/src/runtime/metrics/histogram.go
func sub(a, b *metrics.Float64Histogram) (*metrics.Float64Histogram, bool) {
	equal := true
	res := &metrics.Float64Histogram{
		Counts:  make([]uint64, len(a.Counts)),
		Buckets: make([]float64, len(a.Buckets)),
	}
	copy(res.Buckets, a.Buckets)
	for i := range res.Counts {
		count := a.Counts[i] - b.Counts[i]
		res.Counts[i] = count
		if equal && count != 0 {
			equal = false
		}
	}
	return res, equal
}

func avg(h *metrics.Float64Histogram) float64 {
	var total float64
	var cumulative float64
	for i, count := range h.Counts {
		start, end := h.Buckets[i], h.Buckets[i+1]
		// Handle edge cases where start or end of buckets could be infinity
		if i == 0 && math.IsInf(h.Buckets[0], -1) {
			start = end
		}
		if i == len(h.Counts)-1 && math.IsInf(h.Buckets[len(h.Buckets)-1], 1) {
			end = start
		}
		if start == end && math.IsInf(start, 0) {
			return 0
		}
		cumulative += float64(count) * (float64(start+end) / 2)
		total += float64(count)
	}
	if total == 0 {
		return 0
	}
	return cumulative / total
}

// This function takes a runtime/metrics histogram, and a slice of all
// percentiles to compute for that histogram. It computes all percentiles
// in a single pass and returns the results which is more efficient than
// computing each percentile separately.
func percentiles(h *metrics.Float64Histogram, pInput []float64) []float64 {
	p := make([]float64, len(pInput))
	copy(p, pInput)
	sort.Float64s(p)

	if p[0] < 0.0 || p[len(p)-1] > 1.0 {
		panic("percentiles is invoked with a <0 or >1 percentile")
	}

	results := make([]float64, len(p))

	var total float64 // total count across all buckets
	for i := range h.Counts {
		total += float64(h.Counts[i])
	}

	var cumulative float64 // cumulative count of all buckets we've iterated through
	var start, end float64 // start and end of current bucket
	i := 0                 // index of current bucket
	j := 0                 // index of the percentile we're currently calculating

	for j < len(p) && i < len(h.Counts) {
		start, end = h.Buckets[i], h.Buckets[i+1]
		// Avoid interpolating with Inf if our percentile lies in an edge bucket
		if i == 0 && math.IsInf(h.Buckets[0], -1) {
			start = end
		}
		if i == len(h.Counts)-1 && math.IsInf(h.Buckets[len(h.Buckets)-1], 1) {
			end = start
		}

		if start == end && math.IsInf(start, 0) {
			return results
		}

		// adds the counts of this bucket, to check whether the percentile is in this bucket
		bucketCount := float64(h.Counts[i])
		cumulative += bucketCount

		// Skip empty buckets at the beginning of the histogram and as long as we still have
		// percentiles to compute, check whether the target percentile falls in this bucket
		for (cumulative > 0) && j < len(p) && (cumulative >= total*p[j]) {
			// The target percentile is somewhere in the current bucket: [start, end]
			// and corresponds to a count in: [cumulative-bucketCount, cumulative]
			// We use linear interpolation to estimate the value of the percentile
			// within the bucket.
			//
			//                             bucketCount
			//                  <--------------------------------->
			//                     percentileCount
			//                  <------------------->
			//                  |....................@.............|
			//                  ^                    ^             ^
			// counts:  cumulative-bucketCount | total*p[j] |  cumulative
			//                                 |            |
			// buckets:       start            | percentile |     end
			//
			percentileCount := total*p[j] - (cumulative - bucketCount)
			results[j] = start + (end-start)*(percentileCount/bucketCount) // percentile
			// we can have multiple percentiles fall in the same bucket, so we check if the
			// next percentile falls in this bucket
			j++
		}
		i++
	}

	orderedResults := make([]float64, len(p))
	for i := range orderedResults {
		orderedResults[i] = results[slices.Index(p, pInput[i])]
	}

	return orderedResults
}
