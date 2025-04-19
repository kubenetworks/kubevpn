package runtimemetrics

import (
	"fmt"
	"math"
	"runtime/metrics"
)

const gogcMetricName = "/gc/gogc:percent"
const gomemlimitMetricName = "/gc/gomemlimit:bytes"
const gomaxProcsMetricName = "/sched/gomaxprocs:threads"

func getBaseTags() []string {
	samples := []metrics.Sample{
		{Name: gogcMetricName},
		{Name: gomemlimitMetricName},
		{Name: gomaxProcsMetricName},
	}

	baseTags := make([]string, 0, len(samples))

	metrics.Read(samples)

	for _, s := range samples {
		switch s.Name {
		case gogcMetricName:
			gogc := s.Value.Uint64()
			var goGCTagValue string
			if gogc == math.MaxUint64 {
				goGCTagValue = "off"
			} else {
				goGCTagValue = fmt.Sprintf("%d", gogc)
			}
			baseTags = append(baseTags, fmt.Sprintf("gogc:%s", goGCTagValue))
		case gomemlimitMetricName:
			gomemlimit := s.Value.Uint64()
			var goMemLimitTagValue string
			if gomemlimit == math.MaxInt64 {
				goMemLimitTagValue = "unlimited"
			} else {
				// Convert GOMEMLIMIT to a human-readable string with the right byte unit
				goMemLimitTagValue = formatByteSize(gomemlimit)
			}
			baseTags = append(baseTags, fmt.Sprintf("gomemlimit:%s", goMemLimitTagValue))
		case gomaxProcsMetricName:
			gomaxprocs := s.Value.Uint64()
			baseTags = append(baseTags, fmt.Sprintf("gomaxprocs:%d", gomaxprocs))
		}
	}

	return baseTags
}

// Function to format byte size with the right unit
func formatByteSize(bytes uint64) string {
	const (
		unit   = 1024
		format = "%.0f %sB"
	)
	if bytes < unit {
		return fmt.Sprintf(format, float64(bytes), "")
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf(format, float64(bytes)/float64(div), string("KMGTPE"[exp])+"i")
}
