// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package trace

import (
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/config"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter/trace"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/listener"
)

// AppSec-specific span tags that are expected to
// be in the web service entry span (span of type `web`) when AppSec is enabled.
var staticAppsecTags = map[string]any{
	"_dd.appsec.enabled": 1,
	"_dd.runtime_family": "go",
}

type AppsecSpanTransport struct{}

func (*AppsecSpanTransport) String() string {
	return "Appsec Span Transport"
}

func (*AppsecSpanTransport) Stop() {}

func NewAppsecSpanTransport(_ *config.Config, rootOp dyngo.Operation) (listener.Feature, error) {
	ast := &AppsecSpanTransport{}

	dyngo.On(rootOp, ast.OnServiceEntryStart)
	dyngo.On(rootOp, ast.OnSpanStart)

	return ast, nil
}

// OnServiceEntryStart is the start listener of the trace.ServiceEntrySpanOperation start event.
// It listens for tags and serializable tags and sets them on the span when finishing the operation.
func (*AppsecSpanTransport) OnServiceEntryStart(op *trace.ServiceEntrySpanOperation, _ trace.ServiceEntrySpanArgs) {
	op.SetTags(staticAppsecTags)
	dyngo.OnData(op, op.OnSpanTagEvent)
	dyngo.OnData(op, op.OnServiceEntrySpanTagEvent)
	dyngo.OnData(op, op.OnJSONServiceEntrySpanTagEvent)
	dyngo.OnData(op, op.OnServiceEntrySpanTagsBulkEvent)
}

// OnSpanStart is the start listener of the trace.SpanOperation start event.
// It listens for tags and sets them on the current span when finishing the operation.
func (*AppsecSpanTransport) OnSpanStart(op *trace.SpanOperation, _ trace.SpanArgs) {
	dyngo.OnData(op, op.OnSpanTagEvent)
}
