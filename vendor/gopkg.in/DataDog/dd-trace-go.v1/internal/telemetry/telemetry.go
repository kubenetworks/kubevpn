// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

// Package telemetry implements a client for sending telemetry information to
// Datadog regarding usage of an APM library such as tracing or profiling.
package telemetry

import (
	"time"
)

// ProductChange signals that the product has changed with some configuration
// information. It will start the telemetry client if it is not already started,
// unless enabled is false (in which case the call does nothing). ProductChange
// assumes that the telemetry client has been configured already by the caller
// using the ApplyOps method.
// If the client is already started, it will send any necessary
// app-product-change events to indicate whether the product is enabled, as well
// as an app-client-configuration-change event in case any new configuration
// information is available.
func (c *client) ProductChange(namespace Namespace, enabled bool, configuration []Configuration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		if !enabled {
			// Namespace is not enabled & telemetry isn't started, won't start it now.
			return
		}
		c.start(configuration, namespace)
		return
	}
	c.configChange(configuration)
	switch namespace {
	case NamespaceTracers, NamespaceProfilers, NamespaceAppSec:
		c.productChange(namespace, enabled)
	default:
		log("unknown product namespace %q provided to ProductChange", namespace)
	}
}

// ConfigChange is a thread-safe method to enqueue an app-client-configuration-change event.
func (c *client) ConfigChange(configuration []Configuration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.configChange(configuration)
}

// configChange enqueues an app-client-configuration-change event to be flushed.
// Must be called with c.mu locked.
func (c *client) configChange(configuration []Configuration) {
	if !c.started {
		log("attempted to send config change event, but telemetry client has not started")
		return
	}
	if len(configuration) > 0 {
		configChange := new(ConfigurationChange)
		configChange.Configuration = configuration
		configReq := c.newRequest(RequestTypeAppClientConfigurationChange)
		configReq.Body.Payload = configChange
		c.scheduleSubmit(configReq)
	}
}

// productChange enqueues an app-product-change event that signals a product has been `enabled`.
// Must be called with c.mu locked. An app-product-change event with enabled=true indicates
// that a certain product has been used for this application.
func (c *client) productChange(namespace Namespace, enabled bool) {
	if !c.started {
		log("attempted to send product change event, but telemetry client has not started")
		return
	}
	products := new(ProductsPayload)
	switch namespace {
	case NamespaceAppSec:
		products.Products.AppSec = ProductDetails{Enabled: enabled}
	case NamespaceProfilers:
		products.Products.Profiler = ProductDetails{Enabled: enabled}
	case NamespaceTracers:
		// Nothing to do
	default:
		log("unknown product namespace: %q. The app-product-change telemetry event will not send", namespace)
		return
	}
	productReq := c.newRequest(RequestTypeAppProductChange)
	productReq.Body.Payload = products
	c.scheduleSubmit(productReq)
}

// Integrations returns which integrations are tracked by telemetry.
func Integrations() []Integration {
	contrib.Lock()
	defer contrib.Unlock()
	return contribPackages
}

// LoadIntegration notifies telemetry that an integration is being used.
func LoadIntegration(name string) {
	if Disabled() {
		return
	}
	contrib.Lock()
	defer contrib.Unlock()
	contribPackages = append(contribPackages, Integration{Name: name, Enabled: true})
}

// Time is used to track a distribution metric that measures the time (ms)
// of some portion of code. It returns a function that should be called when
// the desired code finishes executing.
// For example, by adding:
// defer Time(namespace, "init_time", nil, true)()
// at the beginning of the tracer Start function, the tracer start time is measured
// and stored as a metric to be flushed by the global telemetry client.
func Time(namespace Namespace, name string, tags []string, common bool) (finish func()) {
	start := time.Now()
	return func() {
		elapsed := time.Since(start)
		GlobalClient.Record(namespace, MetricKindDist, name, float64(elapsed.Milliseconds()), tags, common)
	}
}
