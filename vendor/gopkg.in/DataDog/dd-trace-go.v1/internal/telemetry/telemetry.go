// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

// Package telemetry implements a client for sending telemetry information to
// Datadog regarding usage of an APM library such as tracing or profiling.
package telemetry

import (
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec"
)

// ProductStart signals that the product has started with some configuration
// information. It will start the telemetry client if it is not already started.
// ProductStart assumes that the telemetry client has been configured already by the caller using the ApplyOps method.
// If the client is already started, it will send any necessary app-product-change
// events to indicate whether the product is enabled, as well as an app-client-configuration-change
// event in case any new configuration information is available.
func (c *client) ProductStart(namespace Namespace, configuration []Configuration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		c.start(configuration, namespace)
		return
	}
	c.configChange(configuration)
	switch namespace {
	case NamespaceProfilers, NamespaceASM:
		c.productEnabled(namespace)
	case NamespaceTracers:
		// Since appsec is integrated with the tracer, we sent an app-product-change
		// update about appsec when the tracer starts. Any tracer-related configuration
		// information can be passed along here as well.
		if appsec.Enabled() {
			c.productEnabled(NamespaceASM)
		}
	default:
		log("unknown product namespace provided to ProductStart")
	}
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

// productEnabled enqueues an app-product-change event that signals a product has been turned on.
// Must be called with c.mu locked. An app-product-change event with enabled=true indicates
// that a certain product has been used for this application.
func (c *client) productEnabled(namespace Namespace) {
	if !c.started {
		log("attempted to send product change event, but telemetry client has not started")
		return
	}
	products := new(Products)
	switch namespace {
	case NamespaceProfilers:
		products.Profiler = ProductDetails{Enabled: true}
	case NamespaceASM:
		products.AppSec = ProductDetails{Enabled: true}
	default:
		log("unknown product namespace, app-product-change telemetry event will not send")
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
