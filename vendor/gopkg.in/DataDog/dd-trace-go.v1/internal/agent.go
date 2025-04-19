// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

package internal

import (
	"net"
	"net/url"
	"os"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

const (
	DefaultAgentHostname  = "localhost"
	DefaultTraceAgentPort = "8126"
)

// This is a variable rather than a constant so it can be replaced in unit tests
var DefaultTraceAgentUDSPath = "/var/run/datadog/apm.socket"

// AgentURLFromEnv resolves the URL for the trace agent based on
// the default host/port and UDS path, and via standard environment variables.
// AgentURLFromEnv has the following priority order:
//   - First, DD_TRACE_AGENT_URL if it is set
//   - Then, if either of DD_AGENT_HOST and DD_TRACE_AGENT_PORT are set,
//     use http://DD_AGENT_HOST:DD_TRACE_AGENT_PORT,
//     defaulting to localhost and 8126, respectively
//   - Then, DefaultTraceAgentUDSPath, if the path exists
//   - Finally, localhost:8126
func AgentURLFromEnv() *url.URL {
	if agentURL := os.Getenv("DD_TRACE_AGENT_URL"); agentURL != "" {
		u, err := url.Parse(agentURL)
		if err != nil {
			log.Warn("Failed to parse DD_TRACE_AGENT_URL: %v", err)
		} else {
			switch u.Scheme {
			case "unix", "http", "https":
				return u
			default:
				log.Warn("Unsupported protocol %q in Agent URL %q. Must be one of: http, https, unix.", u.Scheme, agentURL)
			}
		}
	}

	host, providedHost := os.LookupEnv("DD_AGENT_HOST")
	port, providedPort := os.LookupEnv("DD_TRACE_AGENT_PORT")
	if host == "" {
		// We treat set but empty the same as unset
		providedHost = false
		host = DefaultAgentHostname
	}
	if port == "" {
		// We treat set but empty the same as unset
		providedPort = false
		port = DefaultTraceAgentPort
	}
	httpURL := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, port),
	}
	if providedHost || providedPort {
		return httpURL
	}

	if _, err := os.Stat(DefaultTraceAgentUDSPath); err == nil {
		return &url.URL{
			Scheme: "unix",
			Path:   DefaultTraceAgentUDSPath,
		}
	}
	return httpURL
}
