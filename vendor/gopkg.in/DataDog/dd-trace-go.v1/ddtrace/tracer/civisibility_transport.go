// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package tracer

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	pb "github.com/DataDog/datadog-agent/pkg/proto/pbgo/trace"
	"gopkg.in/DataDog/dd-trace-go.v1/internal"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/civisibility/constants"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/civisibility/utils/telemetry"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/version"
)

// Constants for CI Visibility API paths and subdomains.
const (
	TestCycleSubdomain = "citestcycle-intake" // Subdomain for test cycle intake.
	TestCyclePath      = "api/v2/citestcycle" // API path for test cycle.
	EvpProxyPath       = "evp_proxy/v2"       // Path for EVP proxy.
)

// Ensure that civisibilityTransport implements the transport interface.
var _ transport = (*ciVisibilityTransport)(nil)

// ciVisibilityTransport is a structure that handles sending CI Visibility payloads
// to the Datadog endpoint, either in agentless mode or through the EVP proxy.
type ciVisibilityTransport struct {
	config           *config           // Configuration for the tracer.
	testCycleURLPath string            // URL path for the test cycle endpoint.
	headers          map[string]string // HTTP headers to be included in the requests.
	agentless        bool              // Gets if the transport is configured in agentless mode (eg: Gzip support)
}

// newCiVisibilityTransport creates and initializes a new civisibilityTransport
// based on the provided tracer configuration. It sets up the appropriate headers
// and determines the URL path based on whether agentless mode is enabled.
//
// Parameters:
//
//	config - The tracer configuration.
//
// Returns:
//
//	A pointer to an initialized civisibilityTransport instance.
func newCiVisibilityTransport(config *config) *ciVisibilityTransport {
	// Initialize the default headers with encoder metadata.
	defaultHeaders := map[string]string{
		"Datadog-Meta-Lang":             "go",
		"Datadog-Meta-Lang-Version":     strings.TrimPrefix(runtime.Version(), "go"),
		"Datadog-Meta-Lang-Interpreter": runtime.Compiler + "-" + runtime.GOARCH + "-" + runtime.GOOS,
		"Datadog-Meta-Tracer-Version":   version.Tag,
		"Content-Type":                  "application/msgpack",
	}
	if cid := internal.ContainerID(); cid != "" {
		defaultHeaders["Datadog-Container-ID"] = cid
	}
	if eid := internal.EntityID(); eid != "" {
		defaultHeaders["Datadog-Entity-ID"] = eid
	}

	// Determine if agentless mode is enabled through an environment variable.
	agentlessEnabled := internal.BoolEnv(constants.CIVisibilityAgentlessEnabledEnvironmentVariable, false)

	testCycleURL := ""
	if agentlessEnabled {
		// Agentless mode is enabled.
		APIKeyValue := os.Getenv(constants.APIKeyEnvironmentVariable)
		if APIKeyValue == "" {
			log.Error("An API key is required for agentless mode. Use the DD_API_KEY env variable to set it")
		}

		defaultHeaders["dd-api-key"] = APIKeyValue

		// Check for a custom agentless URL.
		agentlessURL := ""
		if v := os.Getenv(constants.CIVisibilityAgentlessURLEnvironmentVariable); v != "" {
			agentlessURL = v
		}

		if agentlessURL == "" {
			// Use the standard agentless URL format.
			site := "datadoghq.com"
			if v := os.Getenv("DD_SITE"); v != "" {
				site = v
			}

			testCycleURL = fmt.Sprintf("https://%s.%s/%s", TestCycleSubdomain, site, TestCyclePath)
		} else {
			// Use the custom agentless URL.
			testCycleURL = fmt.Sprintf("%s/%s", agentlessURL, TestCyclePath)
		}
	} else {
		// Use agent mode with the EVP proxy.
		defaultHeaders["X-Datadog-EVP-Subdomain"] = TestCycleSubdomain
		testCycleURL = fmt.Sprintf("%s/%s/%s", config.agentURL.String(), EvpProxyPath, TestCyclePath)
	}

	log.Debug("ciVisibilityTransport: creating transport instance [agentless: %v, testcycleurl: %v]", agentlessEnabled, testCycleURL)

	return &ciVisibilityTransport{
		config:           config,
		testCycleURLPath: testCycleURL,
		headers:          defaultHeaders,
		agentless:        agentlessEnabled,
	}
}

// send sends the CI Visibility payload to the Datadog endpoint.
// It prepares the payload, creates the HTTP request, and handles the response.
//
// Parameters:
//
//	p - The payload to be sent.
//
// Returns:
//
//	An io.ReadCloser for reading the response body, and an error if the operation fails.
func (t *ciVisibilityTransport) send(p *payload) (body io.ReadCloser, err error) {
	ciVisibilityPayload := &ciVisibilityPayload{p, 0}
	buffer, bufferErr := ciVisibilityPayload.getBuffer(t.config)
	if bufferErr != nil {
		return nil, fmt.Errorf("cannot create buffer payload: %v", bufferErr)
	}

	if t.agentless {
		// Compress payload
		var gzipBuffer bytes.Buffer
		gzipWriter := gzip.NewWriter(&gzipBuffer)
		_, err = io.Copy(gzipWriter, buffer)
		if err != nil {
			return nil, fmt.Errorf("cannot compress request body: %v", err)
		}
		err = gzipWriter.Close()
		if err != nil {
			return nil, fmt.Errorf("cannot compress request body: %v", err)
		}
		buffer = &gzipBuffer
	}

	req, err := http.NewRequest("POST", t.testCycleURLPath, buffer)
	if err != nil {
		return nil, fmt.Errorf("cannot create http request: %v", err)
	}
	for header, value := range t.headers {
		req.Header.Set(header, value)
	}
	if t.agentless {
		req.Header.Set("Content-Encoding", "gzip")
	}

	log.Debug("ciVisibilityTransport: sending transport request: %v bytes", buffer.Len())
	startTime := time.Now()
	response, err := t.config.httpClient.Do(req)
	telemetry.EndpointPayloadRequestsMs(telemetry.TestCycleEndpointType, float64(time.Since(startTime).Milliseconds()))
	if err != nil {
		return nil, err
	}
	if code := response.StatusCode; code >= 400 {
		// error, check the body for context information and
		// return a nice error.
		msg := make([]byte, 1000)
		n, _ := response.Body.Read(msg)
		_ = response.Body.Close()
		txt := http.StatusText(code)
		telemetry.EndpointPayloadRequestsErrors(telemetry.TestCycleEndpointType, telemetry.GetErrorTypeFromStatusCode(code))
		if n > 0 {
			return nil, fmt.Errorf("%s (Status: %s)", msg[:n], txt)
		}
		return nil, fmt.Errorf("%s", txt)
	}
	return response.Body, nil
}

// sendStats is a no-op for CI Visibility transport as it does not support sending stats payloads.
//
// Parameters:
//
//	payload - The stats payload to be sent.
//
// Returns:
//
//	An error indicating that stats are not supported.
func (t *ciVisibilityTransport) sendStats(*pb.ClientStatsPayload) error {
	// Stats are not supported by CI Visibility agentless / EVP proxy.
	return nil
}

// endpoint returns the URL path of the test cycle endpoint.
//
// Returns:
//
//	The URL path as a string.
func (t *ciVisibilityTransport) endpoint() string {
	return t.testCycleURLPath
}
