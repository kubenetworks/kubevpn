// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package telemetry

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/globalconfig"
)

// An Option is used to configure the telemetry client's settings
type Option func(*client)

// ApplyOps sets various fields of the client.
// To be called before starting any product.
func (c *client) ApplyOps(opts ...Option) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, opt := range opts {
		opt(c)
	}
}

// WithNamespace sets name as the telemetry client's namespace (tracer, profiler, appsec)
func WithNamespace(name Namespace) Option {
	return func(client *client) {
		client.Namespace = name
	}
}

// WithEnv sets the app specific environment for the telemetry client
func WithEnv(env string) Option {
	return func(client *client) {
		client.Env = env
	}
}

// WithService sets the app specific service for the telemetry client
func WithService(service string) Option {
	return func(client *client) {
		client.Service = service
	}
}

// WithVersion sets the app specific version for the telemetry client
func WithVersion(version string) Option {
	return func(client *client) {
		client.Version = version
	}
}

// WithHTTPClient specifies the http client for the telemetry client
func WithHTTPClient(httpClient *http.Client) Option {
	return func(client *client) {
		client.Client = httpClient
	}
}

func defaultAPIKey() string {
	return os.Getenv("DD_API_KEY")
}

// WithAPIKey sets the DD API KEY for the telemetry client
func WithAPIKey(v string) Option {
	return func(client *client) {
		client.APIKey = v
	}
}

// WithURL sets the URL for where telemetry information is flushed to.
// For the URL, uploading through agent goes through
//
//	${AGENT_URL}/telemetry/proxy/api/v2/apmtelemetry
//
// for agentless:
//
//	https://instrumentation-telemetry-intake.datadoghq.com/api/v2/apmtelemetry
//
// with an API key
func WithURL(agentless bool, agentURL string) Option {
	return func(client *client) {
		if agentless {
			client.URL = getAgentlessURL()
		} else {
			u, err := url.Parse(agentURL)
			if err == nil {
				u.Path = "/telemetry/proxy/api/v2/apmtelemetry"
				client.URL = u.String()
			} else {
				log("Agent URL %s is invalid, switching to agentless telemetry endpoint", agentURL)
				client.URL = getAgentlessURL()
			}
		}
	}
}

func getAgentlessURL() string {
	agentlessEndpointLock.RLock()
	defer agentlessEndpointLock.RUnlock()
	return agentlessURL
}

// configEnvFallback returns the value of environment variable with the
// given key if def == ""
func configEnvFallback(key, def string) string {
	if def != "" {
		return def
	}
	return os.Getenv(key)
}

// fallbackOps populates missing fields of the client with environment variables
// or default values.
func (c *client) fallbackOps() error {
	if c.Client == nil {
		WithHTTPClient(defaultHTTPClient)(c)
	}
	if len(c.APIKey) == 0 && c.URL == getAgentlessURL() {
		WithAPIKey(defaultAPIKey())(c)
		if c.APIKey == "" {
			return errors.New("agentless is turned on, but valid DD API key was not found")
		}
	}
	c.Service = configEnvFallback("DD_SERVICE", c.Service)
	if len(c.Service) == 0 {
		if name := globalconfig.ServiceName(); len(name) != 0 {
			c.Service = name
		} else {
			c.Service = filepath.Base(os.Args[0])

		}
	}
	c.Env = configEnvFallback("DD_ENV", c.Env)
	c.Version = configEnvFallback("DD_VERSION", c.Version)
	return nil
}
