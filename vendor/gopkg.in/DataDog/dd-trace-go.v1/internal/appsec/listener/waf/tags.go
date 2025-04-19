// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package waf

import (
	"encoding/json"

	waf "github.com/DataDog/go-libddwaf/v3"

	"gopkg.in/DataDog/dd-trace-go.v1/internal"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/ext"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter/trace"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/samplernames"
)

const (
	wafSpanTagPrefix     = "_dd.appsec."
	eventRulesVersionTag = wafSpanTagPrefix + "event_rules.version"
	eventRulesErrorsTag  = wafSpanTagPrefix + "event_rules.errors"
	eventRulesLoadedTag  = wafSpanTagPrefix + "event_rules.loaded"
	eventRulesFailedTag  = wafSpanTagPrefix + "event_rules.error_count"
	wafVersionTag        = wafSpanTagPrefix + "waf.version"

	// BlockedRequestTag used to convey whether a request is blocked
	BlockedRequestTag = "appsec.blocked"
)

// AddRulesMonitoringTags adds the tags related to security rules monitoring
func AddRulesMonitoringTags(th trace.TagSetter, wafDiags waf.Diagnostics) {
	rInfo := wafDiags.Rules
	if rInfo == nil {
		return
	}

	var rulesetErrors []byte
	var err error
	rulesetErrors, err = json.Marshal(wafDiags.Rules.Errors)
	if err != nil {
		log.Error("appsec: could not marshal the waf ruleset info errors to json")
	}
	th.SetTag(eventRulesErrorsTag, string(rulesetErrors))
	th.SetTag(eventRulesLoadedTag, len(rInfo.Loaded))
	th.SetTag(eventRulesFailedTag, len(rInfo.Failed))
	th.SetTag(wafVersionTag, waf.Version())
	th.SetTag(ext.ManualKeep, samplernames.AppSec)
}

// AddWAFMonitoringTags adds the tags related to the monitoring of the Feature
func AddWAFMonitoringTags(th trace.TagSetter, rulesVersion string, stats map[string]any) {
	// Rules version is set for every request to help the backend associate Feature duration metrics with rule version
	th.SetTag(eventRulesVersionTag, rulesVersion)

	// Report the stats sent by the Feature
	for k, v := range stats {
		th.SetTag(wafSpanTagPrefix+k, v)
	}
}

// SetEventSpanTags sets the security event span tags related to an appsec event
func SetEventSpanTags(span trace.TagSetter) {
	// Keep this span due to the security event
	//
	// This is a workaround to tell the tracer that the trace was kept by AppSec.
	// Passing any other value than `appsec.SamplerAppSec` has no effect.
	// Customers should use `span.SetTag(ext.ManualKeep, true)` pattern
	// to keep the trace, manually.
	span.SetTag(ext.ManualKeep, samplernames.AppSec)
	span.SetTag("_dd.origin", "appsec")
	// Set the appsec.event tag needed by the appsec backend
	span.SetTag("appsec.event", true)
	span.SetTag("_dd.p.appsec", internal.PropagatingTagValue{Value: "1"})
}
