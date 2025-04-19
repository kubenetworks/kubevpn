// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package tracer

import (
	"bytes"
	"sync/atomic"
	"time"

	"github.com/tinylib/msgp/msgp"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/civisibility/constants"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/civisibility/utils"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/civisibility/utils/telemetry"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/globalconfig"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/version"
)

// ciVisibilityPayload represents a payload specifically designed for CI Visibility events.
// It embeds the generic payload structure and adds methods to handle CI Visibility specific data.
type ciVisibilityPayload struct {
	*payload
	serializationTime time.Duration
}

// push adds a new CI Visibility event to the payload buffer.
// It grows the buffer to accommodate the new event, encodes the event in MessagePack format, and updates the event count.
//
// Parameters:
//
//	event - The CI Visibility event to be added to the payload.
//
// Returns:
//
//	An error if encoding the event fails.
func (p *ciVisibilityPayload) push(event *ciVisibilityEvent) error {
	p.buf.Grow(event.Msgsize())
	startTime := time.Now()
	defer func() {
		p.serializationTime += time.Since(startTime)
	}()
	if err := msgp.Encode(&p.buf, event); err != nil {
		return err
	}
	atomic.AddUint32(&p.count, 1)
	p.updateHeader()
	return nil
}

// newCiVisibilityPayload creates a new instance of civisibilitypayload.
//
// Returns:
//
//	A pointer to a newly initialized civisibilitypayload instance.
func newCiVisibilityPayload() *ciVisibilityPayload {
	log.Debug("ciVisibilityPayload: creating payload instance")
	return &ciVisibilityPayload{newPayload(), 0}
}

// getBuffer retrieves the complete body of the CI Visibility payload, including metadata.
// It reads the current payload buffer, adds metadata, and encodes the entire payload in MessagePack format.
//
// Parameters:
//
//	config - A pointer to the config structure containing environment settings.
//
// Returns:
//
//	A pointer to a bytes.Buffer containing the encoded CI Visibility payload.
//	An error if reading from the buffer or encoding the payload fails.
func (p *ciVisibilityPayload) getBuffer(config *config) (*bytes.Buffer, error) {
	startTime := time.Now()
	log.Debug("ciVisibilityPayload: .getBuffer (count: %v)", p.itemCount())

	// Create a buffer to read the current payload
	payloadBuf := new(bytes.Buffer)
	if _, err := payloadBuf.ReadFrom(p.payload); err != nil {
		return nil, err
	}

	// Create the visibility payload
	visibilityPayload := p.writeEnvelope(config.env, payloadBuf.Bytes())

	// Create a new buffer to encode the visibility payload in MessagePack format
	encodedBuf := new(bytes.Buffer)
	if err := msgp.Encode(encodedBuf, visibilityPayload); err != nil {
		return nil, err
	}

	telemetry.EndpointPayloadEventsCount(telemetry.TestCycleEndpointType, float64(p.itemCount()))
	telemetry.EndpointPayloadBytes(telemetry.TestCycleEndpointType, float64(encodedBuf.Len()))
	telemetry.EndpointEventsSerializationMs(telemetry.TestCycleEndpointType, float64((p.serializationTime + time.Since(startTime)).Milliseconds()))
	return encodedBuf, nil
}

func (p *ciVisibilityPayload) writeEnvelope(env string, events []byte) *ciTestCyclePayload {

	/*
			The Payload format in the CI Visibility protocol is like this:
			{
			    "version": 1,
			    "metadata": {
			      "*": {
			        "runtime-id": "...",
			        "language": "...",
			        "library_version": "...",
			        "env": "..."
			      }
			    },
			    "events": [
			      // ...
			    ]
			}

		The event format can be found in the `civisibility_tslv.go` file in the ciVisibilityEvent documentation
	*/

	// Create the metadata map
	allMetadata := map[string]string{
		"language":        "go",
		"runtime-id":      globalconfig.RuntimeID(),
		"library_version": version.Tag,
	}
	if env != "" {
		allMetadata["env"] = env
	}

	// Create the visibility payload
	visibilityPayload := &ciTestCyclePayload{
		Version: 1,
		Metadata: map[string]map[string]string{
			"*": allMetadata,
		},
		Events: events,
	}

	// Check for the test session name and append the tag at the metadata level
	if testSessionName, ok := utils.GetCITags()[constants.TestSessionName]; ok {
		testSessionMap := map[string]string{
			constants.TestSessionName: testSessionName,
		}
		visibilityPayload.Metadata["test_session_end"] = testSessionMap
		visibilityPayload.Metadata["test_module_end"] = testSessionMap
		visibilityPayload.Metadata["test_suite_end"] = testSessionMap
		visibilityPayload.Metadata["test"] = testSessionMap
	}

	return visibilityPayload
}
