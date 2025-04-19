// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	pb "github.com/DataDog/datadog-agent/pkg/proto/pbgo/trace"

	traceinternal "gopkg.in/DataDog/dd-trace-go.v1/ddtrace/internal"
	"gopkg.in/DataDog/dd-trace-go.v1/internal"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/version"

	"github.com/tinylib/msgp/msgp"
)

const (
	// headerComputedTopLevel specifies that the client has marked top-level spans, when set.
	// Any non-empty value will mean 'yes'.
	headerComputedTopLevel = "Datadog-Client-Computed-Top-Level"
)

var defaultDialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
	DualStack: true,
}

func defaultHTTPClient(timeout time.Duration) *http.Client {
	if timeout == 0 {
		timeout = defaultHTTPTimeout
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           defaultDialer.DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		Timeout: timeout,
	}
}

const (
	defaultHostname    = "localhost"
	defaultPort        = "8126"
	defaultAddress     = defaultHostname + ":" + defaultPort
	defaultURL         = "http://" + defaultAddress
	defaultHTTPTimeout = 10 * time.Second        // defines the current timeout before giving up with the send process
	traceCountHeader   = "X-Datadog-Trace-Count" // header containing the number of traces in the payload
)

// transport is an interface for communicating data to the agent.
type transport interface {
	// send sends the payload p to the agent using the transport set up.
	// It returns a non-nil response body when no error occurred.
	send(p *payload) (body io.ReadCloser, err error)
	// sendStats sends the given stats payload to the agent.
	sendStats(s *pb.ClientStatsPayload) error
	// endpoint returns the URL to which the transport will send traces.
	endpoint() string
}

type httpTransport struct {
	traceURL string            // the delivery URL for traces
	statsURL string            // the delivery URL for stats
	client   *http.Client      // the HTTP client used in the POST
	headers  map[string]string // the Transport headers
}

// newTransport returns a new Transport implementation that sends traces to a
// trace agent at the given url, using a given *http.Client.
//
// In general, using this method is only necessary if you have a trace agent
// running on a non-default port, if it's located on another machine, or when
// otherwise needing to customize the transport layer, for instance when using
// a unix domain socket.
func newHTTPTransport(url string, client *http.Client) *httpTransport {
	// initialize the default EncoderPool with Encoder headers
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
	if extEnv := internal.ExternalEnvironment(); extEnv != "" {
		defaultHeaders["Datadog-External-Env"] = extEnv
	}
	return &httpTransport{
		traceURL: fmt.Sprintf("%s/v0.4/traces", url),
		statsURL: fmt.Sprintf("%s/v0.6/stats", url),
		client:   client,
		headers:  defaultHeaders,
	}
}

func (t *httpTransport) sendStats(p *pb.ClientStatsPayload) error {
	var buf bytes.Buffer
	if err := msgp.Encode(&buf, p); err != nil {
		return err
	}
	req, err := http.NewRequest("POST", t.statsURL, &buf)
	if err != nil {
		return err
	}
	for header, value := range t.headers {
		req.Header.Set(header, value)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	if code := resp.StatusCode; code >= 400 {
		// error, check the body for context information and
		// return a nice error.
		msg := make([]byte, 1000)
		n, _ := resp.Body.Read(msg)
		resp.Body.Close()
		txt := http.StatusText(code)
		if n > 0 {
			return fmt.Errorf("%s (Status: %s)", msg[:n], txt)
		}
		return fmt.Errorf("%s", txt)
	}
	return nil
}

func (t *httpTransport) send(p *payload) (body io.ReadCloser, err error) {
	req, err := http.NewRequest("POST", t.traceURL, p)
	if err != nil {
		return nil, fmt.Errorf("cannot create http request: %v", err)
	}
	req.ContentLength = int64(p.size())
	for header, value := range t.headers {
		req.Header.Set(header, value)
	}
	req.Header.Set(traceCountHeader, strconv.Itoa(p.itemCount()))
	req.Header.Set(headerComputedTopLevel, "yes")
	var tr *tracer
	var haveTracer bool
	if tr, haveTracer = traceinternal.GetGlobalTracer().(*tracer); haveTracer {
		if tr.config.tracingAsTransport || tr.config.canComputeStats() {
			// tracingAsTransport uses this header to disable the trace agent's stats computation
			// while making canComputeStats() always false to also disable client stats computation.
			req.Header.Set("Datadog-Client-Computed-Stats", "yes")
		}
		droppedTraces := int(atomic.SwapUint32(&tr.droppedP0Traces, 0))
		partialTraces := int(atomic.SwapUint32(&tr.partialTraces, 0))
		droppedSpans := int(atomic.SwapUint32(&tr.droppedP0Spans, 0))
		if stats := tr.statsd; stats != nil {
			stats.Count("datadog.tracer.dropped_p0_traces", int64(droppedTraces),
				[]string{fmt.Sprintf("partial:%s", strconv.FormatBool(partialTraces > 0))}, 1)
			stats.Count("datadog.tracer.dropped_p0_spans", int64(droppedSpans), nil, 1)
		}
		req.Header.Set("Datadog-Client-Dropped-P0-Traces", strconv.Itoa(droppedTraces))
		req.Header.Set("Datadog-Client-Dropped-P0-Spans", strconv.Itoa(droppedSpans))
	}
	response, err := t.client.Do(req)
	if err != nil {
		reportAPIErrorsMetric(haveTracer, response, err, tr)
		return nil, err
	}
	if code := response.StatusCode; code >= 400 {
		reportAPIErrorsMetric(haveTracer, response, err, tr)
		// error, check the body for context information and
		// return a nice error.
		msg := make([]byte, 1000)
		n, _ := response.Body.Read(msg)
		response.Body.Close()
		txt := http.StatusText(code)
		if n > 0 {
			return nil, fmt.Errorf("%s (Status: %s)", msg[:n], txt)
		}
		return nil, fmt.Errorf("%s", txt)
	}
	return response.Body, nil
}

func reportAPIErrorsMetric(haveTracer bool, response *http.Response, err error, t *tracer) {
	if !haveTracer {
		return
	}
	var reason string
	if err != nil {
		reason = "network_failure"
	}
	if response != nil {
		reason = fmt.Sprintf("server_response_%d", response.StatusCode)
	}
	t.statsd.Incr("datadog.tracer.api.errors", []string{"reason:" + reason}, 1)
}

func (t *httpTransport) endpoint() string {
	return t.traceURL
}
