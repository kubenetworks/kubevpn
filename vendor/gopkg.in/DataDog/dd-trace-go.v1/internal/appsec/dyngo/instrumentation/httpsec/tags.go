// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package httpsec

import (
	"os"
	"sort"
	"strings"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo/instrumentation"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"

	"github.com/DataDog/appsec-internal-go/httpsec"
	"github.com/DataDog/appsec-internal-go/netip"
)

const (
	// envClientIPHeader is the name of the env var used to specify the IP header to be used for client IP collection.
	envClientIPHeader = "DD_TRACE_CLIENT_IP_HEADER"
)

var (
	// Default list of IP-related headers leveraged to retrieve the public
	// client IP address.
	defaultIPHeaders = []string{
		"x-forwarded-for",
		"x-real-ip",
		"true-client-ip",
		"x-client-ip",
		"x-forwarded",
		"forwarded-for",
		"x-cluster-client-ip",
		"fastly-client-ip",
		"cf-connecting-ip",
		"cf-connecting-ip6",
	}

	// Configured list of IP-related headers leveraged to retrieve the public
	// client IP address. Defined at init-time in the init() function below.
	monitoredClientIPHeadersCfg []string

	// List of HTTP headers we collect and send.
	collectedHTTPHeaders = append(defaultIPHeaders,
		"host",
		"content-length",
		"content-type",
		"content-encoding",
		"content-language",
		"forwarded",
		"via",
		"user-agent",
		"accept",
		"accept-encoding",
		"accept-language")
)

func init() {
	if cfg := os.Getenv(envClientIPHeader); cfg != "" {
		// Collect this header value too
		collectedHTTPHeaders = append(collectedHTTPHeaders, cfg)
		// Set this IP header as the only one to consider for ClientIP()
		monitoredClientIPHeadersCfg = []string{cfg}
	} else {
		monitoredClientIPHeadersCfg = defaultIPHeaders
	}

	// Ensure the list of headers are sorted for sort.SearchStrings()
	sort.Strings(collectedHTTPHeaders[:])
}

// SetSecurityEventsTags sets the AppSec-specific span tags when a security event occurred into the service entry span.
func SetSecurityEventsTags(span instrumentation.TagSetter, events []any) {
	if err := instrumentation.SetEventSpanTags(span, events); err != nil {
		log.Error("appsec: unexpected error while creating the appsec events tags: %v", err)
	}
}

func setSecurityEventsTags(span instrumentation.TagSetter, events []any) error {
	if len(events) == 0 {
		return nil
	}
	return instrumentation.SetEventSpanTags(span, events)
}

// setRequestHeadersTags sets the AppSec-specific request headers span tags.
func setRequestHeadersTags(span instrumentation.TagSetter, headers map[string][]string) {
	setHeadersTags(span, "http.request.headers.", headers)
}

// setResponseHeadersTags sets the AppSec-specific response headers span tags.
func setResponseHeadersTags(span instrumentation.TagSetter, headers map[string][]string) {
	setHeadersTags(span, "http.response.headers.", headers)
}

// setHeadersTags sets the AppSec-specific headers span tags.
func setHeadersTags(span instrumentation.TagSetter, tagPrefix string, headers map[string][]string) {
	for h, v := range NormalizeHTTPHeaders(headers) {
		span.SetTag(tagPrefix+h, v)
	}
}

// NormalizeHTTPHeaders returns the HTTP headers following Datadog's
// normalization format.
func NormalizeHTTPHeaders(headers map[string][]string) (normalized map[string]string) {
	if len(headers) == 0 {
		return nil
	}
	normalized = make(map[string]string)
	for k, v := range headers {
		k = strings.ToLower(k)
		if i := sort.SearchStrings(collectedHTTPHeaders[:], k); i < len(collectedHTTPHeaders) && collectedHTTPHeaders[i] == k {
			normalized[k] = strings.Join(v, ",")
		}
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

// ClientIPTags returns the resulting Datadog span tags `http.client_ip`
// containing the client IP and `network.client.ip` containing the remote IP.
// The tags are present only if a valid ip address has been returned by
// ClientIP().
func ClientIPTags(headers map[string][]string, hasCanonicalHeaders bool, remoteAddr string) (tags map[string]string, clientIP netip.Addr) {
	remoteIP, clientIP := httpsec.ClientIP(headers, hasCanonicalHeaders, remoteAddr, monitoredClientIPHeadersCfg)
	tags = httpsec.ClientIPTags(remoteIP, clientIP)
	return tags, clientIP
}
