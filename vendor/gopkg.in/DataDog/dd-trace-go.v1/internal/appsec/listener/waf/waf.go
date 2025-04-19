// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package waf

import (
	"fmt"
	"sync"
	"time"

	"github.com/DataDog/appsec-internal-go/limiter"
	wafv3 "github.com/DataDog/go-libddwaf/v3"

	"gopkg.in/DataDog/dd-trace-go.v1/internal"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/listener"

	"gopkg.in/DataDog/dd-trace-go.v1/appsec/events"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/config"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter/waf"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter/waf/actions"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/stacktrace"
)

type Feature struct {
	timeout         time.Duration
	limiter         *limiter.TokenTicker
	handle          *wafv3.Handle
	supportedAddrs  config.AddressSet
	reportRulesTags sync.Once

	// Determine if we can use [internal.MetaStructValue] to delegate the WAF events serialization to the trace writer
	// or if we have to use the [SerializableTag] method to serialize the events
	metaStructAvailable bool
}

func NewWAFFeature(cfg *config.Config, rootOp dyngo.Operation) (listener.Feature, error) {
	if ok, err := wafv3.Load(); err != nil {
		// 1. If there is an error and the loading is not ok: log as an unexpected error case and quit appsec
		// Note that we assume here that the test for the unsupported target has been done before calling
		// this method, so it is now considered an error for this method
		if !ok {
			return nil, fmt.Errorf("error while loading libddwaf: %w", err)
		}
		// 2. If there is an error and the loading is ok: log as an informative error where appsec can be used
		log.Error("appsec: non-critical error while loading libddwaf: %v", err)
	}

	newHandle, err := wafv3.NewHandle(cfg.RulesManager.Latest, cfg.Obfuscator.KeyRegex, cfg.Obfuscator.ValueRegex)
	if err != nil {
		return nil, err
	}

	cfg.SupportedAddresses = config.NewAddressSet(newHandle.Addresses())

	tokenTicker := limiter.NewTokenTicker(cfg.TraceRateLimit, cfg.TraceRateLimit)
	tokenTicker.Start()

	feature := &Feature{
		handle:              newHandle,
		timeout:             cfg.WAFTimeout,
		limiter:             tokenTicker,
		supportedAddrs:      cfg.SupportedAddresses,
		metaStructAvailable: cfg.MetaStructAvailable,
	}

	dyngo.On(rootOp, feature.onStart)
	dyngo.OnFinish(rootOp, feature.onFinish)

	return feature, nil
}

func (waf *Feature) onStart(op *waf.ContextOperation, _ waf.ContextArgs) {
	waf.reportRulesTags.Do(func() {
		AddRulesMonitoringTags(op, waf.handle.Diagnostics())
	})

	ctx, err := waf.handle.NewContextWithBudget(waf.timeout)
	if err != nil {
		log.Debug("appsec: failed to create Feature context: %v", err)
	}

	op.SwapContext(ctx)
	op.SetLimiter(waf.limiter)
	op.SetSupportedAddresses(waf.supportedAddrs)

	// Run the WAF with the given address data
	dyngo.OnData(op, op.OnEvent)

	waf.SetupActionHandlers(op)
}

func (*Feature) SetupActionHandlers(op *waf.ContextOperation) {
	// Set the blocking tag on the operation when a blocking event is received
	dyngo.OnData(op, func(*events.BlockingSecurityEvent) {
		log.Debug("appsec: blocking event detected")
		op.SetTag(BlockedRequestTag, true)
	})

	// Register the stacktrace if one is requested by a WAF action
	dyngo.OnData(op, func(action *actions.StackTraceAction) {
		log.Debug("appsec: registering stack trace for security purposes")
		op.AddStackTraces(action.Event)
	})

	dyngo.OnData(op, func(*waf.SecurityEvent) {
		log.Debug("appsec: WAF detected a suspicious event")
		SetEventSpanTags(op)
	})
}

func (waf *Feature) onFinish(op *waf.ContextOperation, _ waf.ContextRes) {
	ctx := op.SwapContext(nil)
	if ctx == nil {
		return
	}

	ctx.Close()

	AddWAFMonitoringTags(op, waf.handle.Diagnostics().Version, ctx.Stats().Metrics())
	if wafEvents := op.Events(); len(wafEvents) > 0 {
		tagValue := map[string][]any{"triggers": wafEvents}
		if waf.metaStructAvailable {
			op.SetTag("appsec", internal.MetaStructValue{Value: tagValue})
		} else {
			op.SetSerializableTag("_dd.appsec.json", tagValue)
		}
	}
	op.SetSerializableTags(op.Derivatives())
	if stacks := op.StackTraces(); len(stacks) > 0 {
		op.SetTag(stacktrace.SpanKey, stacktrace.GetSpanValue(stacks...))
	}
}

func (*Feature) String() string {
	return "Web Application Firewall"
}

func (waf *Feature) Stop() {
	waf.limiter.Stop()
	waf.handle.Close()
}
