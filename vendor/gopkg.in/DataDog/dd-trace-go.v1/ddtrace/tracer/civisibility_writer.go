// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package tracer

import (
	"sync"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/civisibility/utils/telemetry"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

// Constants defining the payload size limits for agentless mode.
const (
	// agentlessPayloadMaxLimit is the maximum payload size allowed, indicating the
	// maximum size of the package that the intake can receive.
	agentlessPayloadMaxLimit = 5 * 1024 * 1024 // 5 MB

	// agentlessPayloadSizeLimit specifies the maximum allowed size of the payload before
	// it triggers a flush to the transport.
	agentlessPayloadSizeLimit = agentlessPayloadMaxLimit / 2
)

// Ensure that ciVisibilityTraceWriter implements the traceWriter interface.
var _ traceWriter = (*ciVisibilityTraceWriter)(nil)

// ciVisibilityTraceWriter is responsible for buffering and sending CI visibility trace data
// to the Datadog backend. It manages the payload size and flushes the data when necessary.
type ciVisibilityTraceWriter struct {
	config  *config              // Configuration for the tracer.
	payload *ciVisibilityPayload // Encodes and buffers events in msgpack format.
	climit  chan struct{}        // Limits the number of concurrent outgoing connections.
	wg      sync.WaitGroup       // Waits for all uploads to finish.
}

// newCiVisibilityTraceWriter creates a new instance of ciVisibilityTraceWriter.
//
// Parameters:
//
//	c - The tracer configuration.
//
// Returns:
//
//	A pointer to an initialized ciVisibilityTraceWriter.
func newCiVisibilityTraceWriter(c *config) *ciVisibilityTraceWriter {
	log.Debug("ciVisibilityTraceWriter: creating trace writer instance")
	return &ciVisibilityTraceWriter{
		config:  c,
		payload: newCiVisibilityPayload(),
		climit:  make(chan struct{}, concurrentConnectionLimit),
	}
}

// add adds a new trace to the payload. If the payload size exceeds the limit,
// it triggers a flush to send the data.
//
// Parameters:
//
//	trace - A slice of spans representing the trace to be added.
func (w *ciVisibilityTraceWriter) add(trace []*span) {
	telemetry.EventsEnqueueForSerialization()
	for _, s := range trace {
		cvEvent := getCiVisibilityEvent(s)
		if err := w.payload.push(cvEvent); err != nil {
			log.Error("ciVisibilityTraceWriter: Error encoding msgpack: %v", err)
		}
		if w.payload.size() > agentlessPayloadSizeLimit {
			w.flush()
		}
	}
}

// stop stops the trace writer, ensuring all data is flushed and all uploads are completed.
func (w *ciVisibilityTraceWriter) stop() {
	w.flush()
	w.wg.Wait()
}

// flush sends the current payload to the transport. It ensures that the payload is reset
// and the resources are freed after the flush operation is completed.
func (w *ciVisibilityTraceWriter) flush() {
	if w.payload.itemCount() == 0 {
		return
	}

	w.wg.Add(1)
	w.climit <- struct{}{}
	oldp := w.payload
	w.payload = newCiVisibilityPayload()

	go func(p *ciVisibilityPayload) {
		defer func(start time.Time) {
			// Once the payload has been used, clear the buffer for garbage
			// collection to avoid a memory leak when references to this object
			// may still be kept by faulty transport implementations or the
			// standard library. See dd-trace-go#976
			p.clear()

			<-w.climit
			w.wg.Done()
		}(time.Now())

		var count, size int
		var err error

		requestCompressedType := telemetry.UncompressedRequestCompressedType
		if ciTransport, ok := w.config.transport.(*ciVisibilityTransport); ok && ciTransport.agentless {
			requestCompressedType = telemetry.CompressedRequestCompressedType
		}
		telemetry.EndpointPayloadRequests(telemetry.TestCycleEndpointType, requestCompressedType)

		for attempt := 0; attempt <= w.config.sendRetries; attempt++ {
			size, count = p.size(), p.itemCount()
			log.Debug("ciVisibilityTraceWriter: sending payload: size: %d events: %d\n", size, count)
			_, err = w.config.transport.send(p.payload)
			if err == nil {
				log.Debug("ciVisibilityTraceWriter: sent events after %d attempts", attempt+1)
				return
			}
			log.Error("ciVisibilityTraceWriter: failure sending events (attempt %d), will retry: %v", attempt+1, err)
			p.reset()
			time.Sleep(w.config.retryInterval)
		}
		log.Error("ciVisibilityTraceWriter: lost %d events: %v", count, err)
		telemetry.EndpointPayloadDropped(telemetry.TestCycleEndpointType)
	}(oldp)
}
