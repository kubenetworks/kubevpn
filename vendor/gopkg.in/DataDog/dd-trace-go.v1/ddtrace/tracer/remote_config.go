// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/globalconfig"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/remoteconfig"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/telemetry"

	"github.com/DataDog/datadog-agent/pkg/remoteconfig/state"
)

type configData struct {
	Action        string    `json:"action"`
	ServiceTarget target    `json:"service_target"`
	LibConfig     libConfig `json:"lib_config"`
}

type target struct {
	Service string `json:"service"`
	Env     string `json:"env"`
}

type libConfig struct {
	Enabled            *bool             `json:"tracing_enabled,omitempty"`
	SamplingRate       *float64          `json:"tracing_sampling_rate,omitempty"`
	TraceSamplingRules *[]rcSamplingRule `json:"tracing_sampling_rules,omitempty"`
	HeaderTags         *headerTags       `json:"tracing_header_tags,omitempty"`
	Tags               *tags             `json:"tracing_tags,omitempty"`
}

type rcTag struct {
	Key       string `json:"key"`
	ValueGlob string `json:"value_glob"`
}

// Sampling rules provided by the remote config define tags differently other than using a map.
type rcSamplingRule struct {
	Service    string     `json:"service"`
	Provenance provenance `json:"provenance"`
	Name       string     `json:"name,omitempty"`
	Resource   string     `json:"resource"`
	Tags       []rcTag    `json:"tags,omitempty"`
	SampleRate float64    `json:"sample_rate"`
}

func convertRemoteSamplingRules(rules *[]rcSamplingRule) *[]SamplingRule {
	if rules == nil {
		return nil
	}
	var convertedRules []SamplingRule
	for _, rule := range *rules {
		if rule.Tags != nil && len(rule.Tags) != 0 {
			tags := make(map[string]*regexp.Regexp, len(rule.Tags))
			tagsStrs := make(map[string]string, len(rule.Tags))
			for _, tag := range rule.Tags {
				tags[tag.Key] = globMatch(tag.ValueGlob)
				tagsStrs[tag.Key] = tag.ValueGlob
			}
			x := SamplingRule{
				Service:    globMatch(rule.Service),
				Name:       globMatch(rule.Name),
				Resource:   globMatch(rule.Resource),
				Rate:       rule.SampleRate,
				Tags:       tags,
				Provenance: rule.Provenance,
				globRule: &jsonRule{
					Name:     rule.Name,
					Service:  rule.Service,
					Resource: rule.Resource,
					Tags:     tagsStrs,
				},
			}

			convertedRules = append(convertedRules, x)
		} else {
			x := SamplingRule{
				Service:    globMatch(rule.Service),
				Name:       globMatch(rule.Name),
				Resource:   globMatch(rule.Resource),
				Rate:       rule.SampleRate,
				Provenance: rule.Provenance,
				globRule:   &jsonRule{Name: rule.Name, Service: rule.Service, Resource: rule.Resource},
			}
			convertedRules = append(convertedRules, x)
		}
	}
	return &convertedRules
}

type headerTags []headerTag

type headerTag struct {
	Header  string `json:"header"`
	TagName string `json:"tag_name"`
}

func (hts *headerTags) toSlice() *[]string {
	if hts == nil {
		return nil
	}
	s := make([]string, len(*hts))
	for i, ht := range *hts {
		s[i] = ht.toString()
	}
	return &s
}

func (ht headerTag) toString() string {
	var sb strings.Builder
	sb.WriteString(ht.Header)
	sb.WriteString(":")
	sb.WriteString(ht.TagName)
	return sb.String()
}

type tags []string

func (t *tags) toMap() *map[string]interface{} {
	if t == nil {
		return nil
	}
	m := make(map[string]interface{}, len(*t))
	for _, tag := range *t {
		if kv := strings.SplitN(tag, ":", 2); len(kv) == 2 {
			m[kv[0]] = kv[1]
		}
	}
	return &m
}

// onRemoteConfigUpdate is a remote config callaback responsible for processing APM_TRACING RC-product updates.
func (t *tracer) onRemoteConfigUpdate(u remoteconfig.ProductUpdate) map[string]state.ApplyStatus {
	statuses := map[string]state.ApplyStatus{}
	if len(u) == 0 {
		return statuses
	}
	removed := func() bool {
		// Returns true if all the values in the update are nil.
		for _, raw := range u {
			if raw != nil {
				return false
			}
		}
		return true
	}
	var telemConfigs []telemetry.Configuration
	if removed() {
		// The remote-config client is signaling that the configuration has been deleted for this product.
		// We re-apply the startup configuration values.
		for path := range u {
			log.Debug("Nil payload from RC. Path: %s.", path)
			statuses[path] = state.ApplyStatus{State: state.ApplyStateAcknowledged}
		}
		log.Debug("Resetting configurations")
		updated := t.config.traceSampleRate.reset()
		if updated {
			telemConfigs = append(telemConfigs, t.config.traceSampleRate.toTelemetry())
		}
		updated = t.config.traceSampleRules.reset()
		if updated {
			telemConfigs = append(telemConfigs, t.config.traceSampleRules.toTelemetry())
		}
		updated = t.config.headerAsTags.reset()
		if updated {
			telemConfigs = append(telemConfigs, t.config.headerAsTags.toTelemetry())
		}
		updated = t.config.globalTags.reset()
		if updated {
			telemConfigs = append(telemConfigs, t.config.globalTags.toTelemetry())
		}
		if !t.config.enabled.current {
			log.Debug("APM Tracing is disabled. Restart the service to enable it.")
		}
		if len(telemConfigs) > 0 {
			log.Debug("Reporting %d configuration changes to telemetry", len(telemConfigs))
			telemetry.GlobalClient.ConfigChange(telemConfigs)
		}
		return statuses
	}
	for path, raw := range u {
		if raw == nil {
			continue
		}
		log.Debug("Processing config from RC. Path: %s. Raw: %s", path, raw)
		var c configData
		if err := json.Unmarshal(raw, &c); err != nil {
			log.Debug("Error while unmarshalling payload for %s: %v. Configuration won't be applied.", path, err)
			statuses[path] = state.ApplyStatus{State: state.ApplyStateError, Error: err.Error()}
			continue
		}
		if c.ServiceTarget.Service != t.config.serviceName {
			log.Debug(
				"Skipping config for service %s. Current service is %s",
				c.ServiceTarget.Service,
				t.config.serviceName,
			)
			statuses[path] = state.ApplyStatus{State: state.ApplyStateError, Error: "service mismatch"}
			continue
		}
		if c.ServiceTarget.Env != t.config.env {
			log.Debug("Skipping config for env %s. Current env is %s", c.ServiceTarget.Env, t.config.env)
			statuses[path] = state.ApplyStatus{State: state.ApplyStateError, Error: "env mismatch"}
			continue
		}
		statuses[path] = state.ApplyStatus{State: state.ApplyStateAcknowledged}
		updated := t.config.traceSampleRate.handleRC(c.LibConfig.SamplingRate)
		if updated {
			telemConfigs = append(telemConfigs, t.config.traceSampleRate.toTelemetry())
		}
		updated = t.config.traceSampleRules.handleRC(convertRemoteSamplingRules(c.LibConfig.TraceSamplingRules))
		if updated {
			telemConfigs = append(telemConfigs, t.config.traceSampleRules.toTelemetry())
		}
		updated = t.config.headerAsTags.handleRC(c.LibConfig.HeaderTags.toSlice())
		if updated {
			telemConfigs = append(telemConfigs, t.config.headerAsTags.toTelemetry())
		}
		updated = t.config.globalTags.handleRC(c.LibConfig.Tags.toMap())
		if updated {
			telemConfigs = append(telemConfigs, t.config.globalTags.toTelemetry())
		}
		if c.LibConfig.Enabled != nil {
			if t.config.enabled.current == true && *c.LibConfig.Enabled == false {
				log.Debug("Disabled APM Tracing through RC. Restart the service to enable it.")
				t.config.enabled.handleRC(c.LibConfig.Enabled)
				telemConfigs = append(telemConfigs, t.config.enabled.toTelemetry())
			} else if t.config.enabled.current == false && *c.LibConfig.Enabled == true {
				log.Debug("APM Tracing is disabled. Restart the service to enable it.")
			}
		}
	}
	if len(telemConfigs) > 0 {
		log.Debug("Reporting %d configuration changes to telemetry", len(telemConfigs))
		telemetry.GlobalClient.ConfigChange(telemConfigs)
	}
	return statuses
}

type dynamicInstrumentationRCProbeConfig struct {
	runtimeID     string
	configPath    string
	configContent string
}

type dynamicInstrumentationRCState struct {
	sync.Mutex
	state map[string]dynamicInstrumentationRCProbeConfig
}

var (
	diRCState   dynamicInstrumentationRCState
	initalizeRC sync.Once
)

func (t *tracer) dynamicInstrumentationRCUpdate(u remoteconfig.ProductUpdate) map[string]state.ApplyStatus {
	applyStatus := map[string]state.ApplyStatus{}

	diRCState.Lock()
	for k, v := range u {
		log.Debug("Received dynamic instrumentation RC configuration for %s\n", k)
		applyStatus[k] = state.ApplyStatus{State: state.ApplyStateUnknown}
		diRCState.state[k] = dynamicInstrumentationRCProbeConfig{
			runtimeID:     globalconfig.RuntimeID(),
			configPath:    k,
			configContent: string(v),
		}
	}
	diRCState.Unlock()
	return applyStatus
}

// passProbeConfiguration is used as a stable interface to find the configuration in via bpf. Go-DI attaches
// a bpf program to this function and extracts the raw bytes accordingly.
//
//nolint:all
//go:noinline
func passProbeConfiguration(runtimeID, configPath, configContent string) {}

func initalizeDynamicInstrumentationRemoteConfigState() {
	diRCState = dynamicInstrumentationRCState{
		state: map[string]dynamicInstrumentationRCProbeConfig{},
	}

	go func() {
		for {
			time.Sleep(time.Second * 5)
			diRCState.Lock()
			for _, v := range diRCState.state {
				accessStringsToMitigatePageFault(v.runtimeID, v.configPath, v.configContent)
				passProbeConfiguration(v.runtimeID, v.configPath, v.configContent)
			}
			diRCState.Unlock()
		}
	}()
}

// accessStringsToMitigatePageFault iterates over each string to trigger a page fault,
// ensuring it is loaded into RAM or listed in the translation lookaside buffer.
// This is done by writing the string to io.Discard.
//
// This function addresses an issue with the bpf program that hooks the
// `passProbeConfiguration()` function from system-probe. The bpf program fails
// to read strings if a page fault occurs because the `bpf_probe_read()` helper
// disables paging (uprobe bpf programs can't sleep). Consequently, page faults
// cause `bpf_probe_read()` to return an error and not read any data.
// By preloading the strings, we mitigate this issue, enhancing the reliability
// of the Go Dynamic Instrumentation product.
func accessStringsToMitigatePageFault(strs ...string) {
	for i := range strs {
		io.WriteString(io.Discard, strs[i])
	}
}

// startRemoteConfig starts the remote config client.
// It registers the APM_TRACING product with a callback,
// and the LIVE_DEBUGGING product without a callback.
func (t *tracer) startRemoteConfig(rcConfig remoteconfig.ClientConfig) error {
	err := remoteconfig.Start(rcConfig)
	if err != nil {
		return err
	}

	var dynamicInstrumentationError, apmTracingError error

	if t.config.dynamicInstrumentationEnabled {
		dynamicInstrumentationError = remoteconfig.Subscribe("LIVE_DEBUGGING", t.dynamicInstrumentationRCUpdate)
	}

	initalizeRC.Do(initalizeDynamicInstrumentationRemoteConfigState)

	apmTracingError = remoteconfig.Subscribe(
		state.ProductAPMTracing,
		t.onRemoteConfigUpdate,
		remoteconfig.APMTracingSampleRate,
		remoteconfig.APMTracingHTTPHeaderTags,
		remoteconfig.APMTracingCustomTags,
		remoteconfig.APMTracingEnabled,
		remoteconfig.APMTracingSampleRules,
	)

	if apmTracingError != nil || dynamicInstrumentationError != nil {
		return fmt.Errorf("could not subscribe to at least one remote config product: %s; %s",
			apmTracingError, dynamicInstrumentationError)
	}

	return nil
}
