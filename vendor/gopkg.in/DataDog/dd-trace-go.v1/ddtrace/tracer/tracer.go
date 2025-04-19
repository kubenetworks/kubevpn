// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	gocontext "context"
	"encoding/binary"
	"log/slog"
	"math"
	"os"
	"runtime/pprof"
	rt "runtime/trace"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/ext"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/internal"
	globalinternal "gopkg.in/DataDog/dd-trace-go.v1/internal"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec"
	appsecConfig "gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/config"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/datastreams"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/hostname"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/remoteconfig"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/samplernames"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/telemetry"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/traceprof"

	"github.com/DataDog/datadog-agent/pkg/obfuscate"
	"github.com/DataDog/go-runtime-metrics-internal/pkg/runtimemetrics"
)

var _ ddtrace.Tracer = (*tracer)(nil)

// tracer creates, buffers and submits Spans which are used to time blocks of
// computation. They are accumulated and streamed into an internal payload,
// which is flushed to the agent whenever its size exceeds a specific threshold
// or when a certain interval of time has passed, whichever happens first.
//
// tracer operates based on a worker loop which responds to various request
// channels. It additionally holds two buffers which accumulates error and trace
// queues to be processed by the payload encoder.
type tracer struct {
	config *config

	// stats specifies the concentrator used to compute statistics, when client-side
	// stats are enabled.
	stats *concentrator

	// traceWriter is responsible for sending finished traces to their
	// destination, such as the Trace Agent or Datadog Forwarder.
	traceWriter traceWriter

	// out receives chunk with spans to be added to the payload.
	out chan *chunk

	// flush receives a channel onto which it will confirm after a flush has been
	// triggered and completed.
	flush chan chan<- struct{}

	// stop causes the tracer to shut down when closed.
	stop chan struct{}

	// stopOnce ensures the tracer is stopped exactly once.
	stopOnce sync.Once

	// wg waits for all goroutines to exit when stopping.
	wg sync.WaitGroup

	// prioritySampling holds an instance of the priority sampler.
	prioritySampling *prioritySampler

	// pid of the process
	pid int

	// These integers track metrics about spans and traces as they are started,
	// finished, and dropped
	spansStarted, spansFinished, tracesDropped uint32

	// Keeps track of the total number of traces dropped for accurate logging.
	totalTracesDropped uint32

	logDroppedTraces *time.Ticker

	// Records the number of dropped P0 traces and spans.
	droppedP0Traces, droppedP0Spans uint32

	// partialTrace the number of partially dropped traces.
	partialTraces uint32

	// rulesSampling holds an instance of the rules sampler used to apply either trace sampling,
	// or single span sampling rules on spans. These are user-defined
	// rules for applying a sampling rate to spans that match the designated service
	// or operation name.
	rulesSampling *rulesSampler

	// obfuscator holds the obfuscator used to obfuscate resources in aggregated stats.
	// obfuscator may be nil if disabled.
	obfuscator *obfuscate.Obfuscator

	// statsd is used for tracking metrics associated with the runtime and the tracer.
	statsd globalinternal.StatsdClient

	// dataStreams processes data streams monitoring information
	dataStreams *datastreams.Processor

	// abandonedSpansDebugger specifies where and how potentially abandoned spans are stored
	// when abandoned spans debugging is enabled.
	abandonedSpansDebugger *abandonedSpansDebugger

	// logFile contains a pointer to the file for writing tracer logs along with helper functionality for closing the file
	// logFile is closed when tracer stops
	// by default, tracer logs to stderr and this setting is unused
	logFile *log.ManagedFile
}

const (
	// flushInterval is the interval at which the payload contents will be flushed
	// to the transport.
	flushInterval = 2 * time.Second

	// payloadMaxLimit is the maximum payload size allowed and should indicate the
	// maximum size of the package that the agent can receive.
	payloadMaxLimit = 9.5 * 1024 * 1024 // 9.5 MB

	// payloadSizeLimit specifies the maximum allowed size of the payload before
	// it will trigger a flush to the transport.
	payloadSizeLimit = payloadMaxLimit / 2

	// concurrentConnectionLimit specifies the maximum number of concurrent outgoing
	// connections allowed.
	concurrentConnectionLimit = 100
)

// statsInterval is the interval at which health metrics will be sent with the
// statsd client; replaced in tests.
var statsInterval = 10 * time.Second

// Start starts the tracer with the given set of options. It will stop and replace
// any running tracer, meaning that calling it several times will result in a restart
// of the tracer by replacing the current instance with a new one.
func Start(opts ...StartOption) {
	if internal.Testing {
		return // mock tracer active
	}
	defer telemetry.Time(telemetry.NamespaceGeneral, "init_time", nil, true)()
	t := newTracer(opts...)
	if !t.config.enabled.current {
		// TODO: instrumentation telemetry client won't get started
		// if tracing is disabled, but we still want to capture this
		// telemetry information. Will be fixed when the tracer and profiler
		// share control of the global telemetry client.
		return
	}
	internal.SetGlobalTracer(t)
	if t.dataStreams != nil {
		t.dataStreams.Start()
	}
	if t.config.ciVisibilityAgentless {
		// CI Visibility agentless mode doesn't require remote configuration.

		// start instrumentation telemetry unless it is disabled through the
		// DD_INSTRUMENTATION_TELEMETRY_ENABLED env var
		startTelemetry(t.config)

		// start appsec
		appsec.Start(t.config.appsecStartOptions...)
		_ = t.hostname() // Prime the hostname cache
		return
	}

	// Start AppSec with remote configuration
	cfg := remoteconfig.DefaultClientConfig()
	cfg.AgentURL = t.config.agentURL.String()
	cfg.AppVersion = t.config.version
	cfg.Env = t.config.env
	cfg.HTTP = t.config.httpClient
	cfg.ServiceName = t.config.serviceName
	if err := t.startRemoteConfig(cfg); err != nil {
		log.Warn("Remote config startup error: %s", err)
	}

	// start instrumentation telemetry unless it is disabled through the
	// DD_INSTRUMENTATION_TELEMETRY_ENABLED env var
	startTelemetry(t.config)

	// appsec.Start() may use the telemetry client to report activation, so it is
	// important this happens _AFTER_ startTelemetry() has been called, so the
	// client is appropriately configured.
	appsecopts := make([]appsecConfig.StartOption, 0, len(t.config.appsecStartOptions)+1)
	appsecopts = append(appsecopts, t.config.appsecStartOptions...)
	appsecopts = append(appsecopts, appsecConfig.WithRCConfig(cfg), appsecConfig.WithMetaStructAvailable(t.config.agent.metaStructAvailable))
	appsec.Start(appsecopts...)

	if t.config.logStartup {
		logStartup(t)
	}

	_ = t.hostname() // Prime the hostname cache
}

// Stop stops the started tracer. Subsequent calls are valid but become no-op.
func Stop() {
	internal.SetGlobalTracer(&internal.NoopTracer{})
	log.Flush()
}

// Span is an alias for ddtrace.Span. It is here to allow godoc to group methods returning
// ddtrace.Span. It is recommended and is considered more correct to refer to this type as
// ddtrace.Span instead.
type Span = ddtrace.Span

// StartSpan starts a new span with the given operation name and set of options.
// If the tracer is not started, calling this function is a no-op.
func StartSpan(operationName string, opts ...StartSpanOption) Span {
	return internal.GetGlobalTracer().StartSpan(operationName, opts...)
}

// Extract extracts a SpanContext from the carrier. The carrier is expected
// to implement TextMapReader, otherwise an error is returned.
// If the tracer is not started, calling this function is a no-op.
func Extract(carrier interface{}) (ddtrace.SpanContext, error) {
	return internal.GetGlobalTracer().Extract(carrier)
}

// Inject injects the given SpanContext into the carrier. The carrier is
// expected to implement TextMapWriter, otherwise an error is returned.
// If the tracer is not started, calling this function is a no-op.
func Inject(ctx ddtrace.SpanContext, carrier interface{}) error {
	return internal.GetGlobalTracer().Inject(ctx, carrier)
}

// SetUser associates user information to the current trace which the
// provided span belongs to. The options can be used to tune which user
// bit of information gets monitored. In case of distributed traces,
// the user id can be propagated across traces using the WithPropagation() option.
// See https://docs.datadoghq.com/security_platform/application_security/setup_and_configure/?tab=set_user#add-user-information-to-traces
func SetUser(s Span, id string, opts ...UserMonitoringOption) {
	if s == nil {
		return
	}
	sp, ok := s.(interface {
		SetUser(string, ...UserMonitoringOption)
	})
	if !ok {
		return
	}
	sp.SetUser(id, opts...)
}

// payloadQueueSize is the buffer size of the trace channel.
const payloadQueueSize = 1000

func newUnstartedTracer(opts ...StartOption) *tracer {
	c := newConfig(opts...)
	sampler := newPrioritySampler()
	statsd, err := newStatsdClient(c)
	if err != nil {
		log.Warn("Runtime and health metrics disabled: %v", err)
	}
	var writer traceWriter
	if c.ciVisibilityEnabled {
		writer = newCiVisibilityTraceWriter(c)
	} else if c.logToStdout {
		writer = newLogTraceWriter(c, statsd)
	} else {
		writer = newAgentTraceWriter(c, sampler, statsd)
	}
	traces, spans, err := samplingRulesFromEnv()
	if err != nil {
		log.Warn("DIAGNOSTICS Error(s) parsing sampling rules: found errors:%s", err)
	}
	if traces != nil {
		c.traceRules = traces
	}
	if spans != nil {
		c.spanRules = spans
	}

	rulesSampler := newRulesSampler(c.traceRules, c.spanRules, c.globalSampleRate, c.traceRateLimitPerSecond)
	c.traceSampleRate = newDynamicConfig("trace_sample_rate", c.globalSampleRate, rulesSampler.traces.setGlobalSampleRate, equal[float64])
	// If globalSampleRate returns NaN, it means the environment variable was not set or valid.
	// We could always set the origin to "env_var" inconditionally, but then it wouldn't be possible
	// to distinguish between the case where the environment variable was not set and the case where
	// it default to NaN.
	if !math.IsNaN(c.globalSampleRate) {
		c.traceSampleRate.cfgOrigin = telemetry.OriginEnvVar
	}
	c.traceSampleRules = newDynamicConfig("trace_sample_rules", c.traceRules,
		rulesSampler.traces.setTraceSampleRules, EqualsFalseNegative)
	var dataStreamsProcessor *datastreams.Processor
	if c.dataStreamsMonitoringEnabled {
		dataStreamsProcessor = datastreams.NewProcessor(statsd, c.env, c.serviceName, c.version, c.agentURL, c.httpClient)
	}
	var logFile *log.ManagedFile
	if v := c.logDirectory; v != "" {
		logFile, err = log.OpenFileAtPath(v)
		if err != nil {
			log.Warn("%v", err)
			c.logDirectory = ""
		}
	}
	t := &tracer{
		config:           c,
		traceWriter:      writer,
		out:              make(chan *chunk, payloadQueueSize),
		stop:             make(chan struct{}),
		flush:            make(chan chan<- struct{}),
		rulesSampling:    rulesSampler,
		prioritySampling: sampler,
		pid:              os.Getpid(),
		logDroppedTraces: time.NewTicker(1 * time.Second),
		stats:            newConcentrator(c, defaultStatsBucketSize, statsd),
		obfuscator: obfuscate.NewObfuscator(obfuscate.Config{
			SQL: obfuscate.SQLConfig{
				TableNames:       c.agent.HasFlag("table_names"),
				ReplaceDigits:    c.agent.HasFlag("quantize_sql_tables") || c.agent.HasFlag("replace_sql_digits"),
				KeepSQLAlias:     c.agent.HasFlag("keep_sql_alias"),
				DollarQuotedFunc: c.agent.HasFlag("dollar_quoted_func"),
			},
		}),
		statsd:      statsd,
		dataStreams: dataStreamsProcessor,
		logFile:     logFile,
	}
	return t
}

// newTracer creates a new no-op tracer for testing.
// NOTE: This function does NOT set the global tracer, which is required for
// most finish span/flushing operations to work as expected. If you are calling
// span.Finish and/or expecting flushing to work, you must call
// internal.SetGlobalTracer(...) with the tracer provided by this function.
func newTracer(opts ...StartOption) *tracer {
	t := newUnstartedTracer(opts...)
	c := t.config
	t.statsd.Incr("datadog.tracer.started", nil, 1)
	if c.runtimeMetrics {
		log.Debug("Runtime metrics enabled.")
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.reportRuntimeMetrics(defaultMetricsReportInterval)
		}()
	}
	if c.runtimeMetricsV2 {
		l := slog.New(slogHandler{})
		if err := runtimemetrics.Start(t.statsd, l); err == nil {
			l.Debug("Runtime metrics v2 enabled.")
		} else {
			l.Error("Failed to enable runtime metrics v2", "err", err.Error())
		}
	}
	if c.debugAbandonedSpans {
		log.Info("Abandoned spans logs enabled.")
		t.abandonedSpansDebugger = newAbandonedSpansDebugger()
		t.abandonedSpansDebugger.Start(t.config.spanTimeout)
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		tick := t.config.tickChan
		if tick == nil {
			ticker := time.NewTicker(flushInterval)
			defer ticker.Stop()
			tick = ticker.C
		}
		t.worker(tick)
	}()
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.reportHealthMetricsAtInterval(statsInterval)
	}()
	t.stats.Start()
	return t
}

// Flush flushes any buffered traces. Flush is in effect only if a tracer
// is started. Users do not have to call Flush in order to ensure that
// traces reach Datadog. It is a convenience method dedicated to a specific
// use case described below.
//
// Flush is of use in Lambda environments, where starting and stopping
// the tracer on each invocation may create too much latency. In this
// scenario, a tracer may be started and stopped by the parent process
// whereas the invocation can make use of Flush to ensure any created spans
// reach the agent.
func Flush() {
	if t, ok := internal.GetGlobalTracer().(*tracer); ok {
		t.flushSync()
		if t.dataStreams != nil {
			t.dataStreams.Flush()
		}
	}
}

// flushSync triggers a flush and waits for it to complete.
func (t *tracer) flushSync() {
	done := make(chan struct{})
	t.flush <- done
	<-done
}

// worker receives finished traces to be added into the payload, as well
// as periodically flushes traces to the transport.
func (t *tracer) worker(tick <-chan time.Time) {
	for {
		select {
		case trace := <-t.out:
			t.sampleChunk(trace)
			if len(trace.spans) != 0 {
				t.traceWriter.add(trace.spans)
			}
		case <-tick:
			t.statsd.Incr("datadog.tracer.flush_triggered", []string{"reason:scheduled"}, 1)
			t.traceWriter.flush()

		case done := <-t.flush:
			t.statsd.Incr("datadog.tracer.flush_triggered", []string{"reason:invoked"}, 1)
			t.traceWriter.flush()
			t.statsd.Flush()
			if !t.config.tracingAsTransport {
				t.stats.flushAndSend(time.Now(), withCurrentBucket)
			}
			// TODO(x): In reality, the traceWriter.flush() call is not synchronous
			// when using the agent traceWriter. However, this functionality is used
			// in Lambda so for that purpose this mechanism should suffice.
			done <- struct{}{}

		case <-t.stop:
		loop:
			// the loop ensures that the payload channel is fully drained
			// before the final flush to ensure no traces are lost (see #526)
			for {
				select {
				case trace := <-t.out:
					t.sampleChunk(trace)
					if len(trace.spans) != 0 {
						t.traceWriter.add(trace.spans)
					}
				default:
					break loop
				}
			}
			return
		}
	}
}

// chunk holds information about a trace chunk to be flushed, including its spans.
// The chunk may be a fully finished local trace chunk, or only a portion of the local trace chunk in the case of
// partial flushing.
type chunk struct {
	spans    []*span
	willSend bool // willSend indicates whether the trace will be sent to the agent.
}

// sampleChunk applies single-span sampling to the provided trace.
func (t *tracer) sampleChunk(c *chunk) {
	if len(c.spans) > 0 {
		if p, ok := c.spans[0].context.SamplingPriority(); ok && p > 0 {
			// The trace is kept, no need to run single span sampling rules.
			return
		}
	}
	var kept []*span
	if t.rulesSampling.HasSpanRules() {
		// Apply sampling rules to individual spans in the trace.
		for _, span := range c.spans {
			if t.rulesSampling.SampleSpan(span) {
				kept = append(kept, span)
			}
		}
		if len(kept) > 0 && len(kept) < len(c.spans) {
			// Some spans in the trace were kept, so a partial trace will be sent.
			atomic.AddUint32(&t.partialTraces, 1)
		}
	}
	atomic.AddUint32(&t.droppedP0Spans, uint32(len(c.spans)-len(kept)))
	if !c.willSend {
		if len(kept) == 0 {
			atomic.AddUint32(&t.droppedP0Traces, 1)
		}
		c.spans = kept
	}
}

func (t *tracer) pushChunk(trace *chunk) {
	select {
	case <-t.stop:
		return
	default:
	}
	select {
	case t.out <- trace:
	default:
		log.Debug("payload queue full, trace dropped %d spans", len(trace.spans))
		atomic.AddUint32(&t.totalTracesDropped, 1)
	}
	select {
	case <-t.logDroppedTraces.C:
		if t := atomic.SwapUint32(&t.totalTracesDropped, 0); t > 0 {
			log.Error("%d traces dropped through payload queue", t)
		}
	default:
	}
}

// StartSpan creates, starts, and returns a new Span with the given `operationName`.
func (t *tracer) StartSpan(operationName string, options ...ddtrace.StartSpanOption) ddtrace.Span {
	if !t.config.enabled.current {
		return internal.NoopSpan{}
	}
	var opts ddtrace.StartSpanConfig
	for _, fn := range options {
		fn(&opts)
	}
	var startTime int64
	if opts.StartTime.IsZero() {
		startTime = now()
	} else {
		startTime = opts.StartTime.UnixNano()
	}
	var context *spanContext
	// The default pprof context is taken from the start options and is
	// not nil when using StartSpanFromContext()
	pprofContext := opts.Context
	if opts.Parent != nil {
		if ctx, ok := opts.Parent.(*spanContext); ok {
			context = ctx
			if pprofContext == nil && ctx.span != nil {
				// Inherit the context.Context from parent span if it was propagated
				// using ChildOf() rather than StartSpanFromContext(), see
				// applyPPROFLabels() below.
				pprofContext = ctx.span.pprofCtxActive
			}
		} else if p, ok := opts.Parent.(ddtrace.SpanContextW3C); ok {
			context = &spanContext{
				traceID: p.TraceID128Bytes(),
				spanID:  p.SpanID(),
			}
		}
	}
	if pprofContext == nil {
		// For root span's without context, there is no pprofContext, but we need
		// one to avoid a panic() in pprof.WithLabels(). Using context.Background()
		// is not ideal here, as it will cause us to remove all labels from the
		// goroutine when the span finishes. However, the alternatives of not
		// applying labels for such spans or to leave the endpoint/hotspot labels
		// on the goroutine after it finishes are even less appealing. We'll have
		// to properly document this for users.
		pprofContext = gocontext.Background()
	}
	id := opts.SpanID
	if id == 0 {
		id = generateSpanID(startTime)
	}
	// span defaults
	span := &span{
		Name:         operationName,
		Service:      t.config.serviceName,
		Resource:     operationName,
		SpanID:       id,
		TraceID:      id,
		Start:        startTime,
		noDebugStack: t.config.noDebugStack,
	}

	span.SpanLinks = append(span.SpanLinks, opts.SpanLinks...)

	if t.config.hostname != "" {
		span.setMeta(keyHostname, t.config.hostname)
	}
	if context != nil {
		// this is a child span
		span.TraceID = context.traceID.Lower()
		span.ParentID = context.spanID
		if p, ok := context.SamplingPriority(); ok {
			span.setMetric(keySamplingPriority, float64(p))
		}
		if context.span != nil {
			// local parent, inherit service
			context.span.RLock()
			span.Service = context.span.Service
			context.span.RUnlock()
		} else {
			// remote parent
			if context.origin != "" {
				// mark origin
				span.setMeta(keyOrigin, context.origin)
			}
		}

		if context.reparentID != "" {
			span.setMeta(keyReparentID, context.reparentID)
		}

	}
	span.context = newSpanContext(span, context)
	span.setMetric(ext.Pid, float64(t.pid))
	span.setMeta("language", "go")

	// add tags from options
	for k, v := range opts.Tags {
		span.SetTag(k, v)
	}
	// add global tags
	for k, v := range t.config.globalTags.get() {
		span.SetTag(k, v)
	}
	if t.config.serviceMappings != nil {
		if newSvc, ok := t.config.serviceMappings[span.Service]; ok {
			span.Service = newSvc
		}
	}
	isRootSpan := context == nil || context.span == nil
	if isRootSpan {
		traceprof.SetProfilerRootTags(span)
		span.setMetric(keySpanAttributeSchemaVersion, float64(t.config.spanAttributeSchemaVersion))
	}
	if isRootSpan || context.span.Service != span.Service {
		span.setMetric(keyTopLevel, 1)
		// all top level spans are measured. So the measured tag is redundant.
		delete(span.Metrics, keyMeasured)
	}
	if t.config.version != "" {
		if t.config.universalVersion || (!t.config.universalVersion && span.Service == t.config.serviceName) {
			span.setMeta(ext.Version, t.config.version)
		}
	}
	if t.config.env != "" {
		span.setMeta(ext.Environment, t.config.env)
	}
	if _, ok := span.context.SamplingPriority(); !ok {
		// if not already sampled or a brand new trace, sample it
		t.sample(span)
	}
	pprofContext, span.taskEnd = startExecutionTracerTask(pprofContext, span)
	if t.config.profilerHotspots || t.config.profilerEndpoints {
		t.applyPPROFLabels(pprofContext, span)
	}
	if t.config.serviceMappings != nil {
		if newSvc, ok := t.config.serviceMappings[span.Service]; ok {
			span.Service = newSvc
		}
	}
	if log.DebugEnabled() {
		// avoid allocating the ...interface{} argument if debug logging is disabled
		log.Debug("Started Span: %v, Operation: %s, Resource: %s, Tags: %v, %v",
			span, span.Name, span.Resource, span.Meta, span.Metrics)
	}
	if t.config.debugAbandonedSpans {
		select {
		case t.abandonedSpansDebugger.In <- newAbandonedSpanCandidate(span, false):
			// ok
		default:
			log.Error("Abandoned spans channel full, disregarding span.")
		}
	}
	return span
}

// applyPPROFLabels applies pprof labels for the profiler's code hotspots and
// endpoint filtering feature to span. When span finishes, any pprof labels
// found in ctx are restored. Additionally, this func informs the profiler how
// many times each endpoint is called.
func (t *tracer) applyPPROFLabels(ctx gocontext.Context, span *span) {
	var labels []string
	if t.config.profilerHotspots {
		// allocate the max-length slice to avoid growing it later
		labels = make([]string, 0, 6)
		labels = append(labels, traceprof.SpanID, strconv.FormatUint(span.SpanID, 10))
	}
	// nil checks might not be needed, but better be safe than sorry
	if localRootSpan := span.root(); localRootSpan != nil {
		if t.config.profilerHotspots {
			labels = append(labels, traceprof.LocalRootSpanID, strconv.FormatUint(localRootSpan.SpanID, 10))
		}
		if t.config.profilerEndpoints && spanResourcePIISafe(localRootSpan) {
			labels = append(labels, traceprof.TraceEndpoint, localRootSpan.Resource)
			if span == localRootSpan {
				// Inform the profiler of endpoint hits. This is used for the unit of
				// work feature. We can't use APM stats for this since the stats don't
				// have enough cardinality (e.g. runtime-id tags are missing).
				traceprof.GlobalEndpointCounter().Inc(localRootSpan.Resource)
			}
		}
	}
	if len(labels) > 0 {
		span.pprofCtxRestore = ctx
		span.pprofCtxActive = pprof.WithLabels(ctx, pprof.Labels(labels...))
		pprof.SetGoroutineLabels(span.pprofCtxActive)
	}
}

// spanResourcePIISafe returns true if s.Resource can be considered to not
// include PII with reasonable confidence. E.g. SQL queries may contain PII,
// but http, rpc or custom (s.Type == "") span resource names generally do not.
func spanResourcePIISafe(s *span) bool {
	return s.Type == ext.SpanTypeWeb || s.Type == ext.AppTypeRPC || s.Type == ""
}

// Stop stops the tracer.
func (t *tracer) Stop() {
	t.stopOnce.Do(func() {
		close(t.stop)
		t.statsd.Incr("datadog.tracer.stopped", nil, 1)
	})
	t.abandonedSpansDebugger.Stop()
	t.stats.Stop()
	t.wg.Wait()
	t.traceWriter.stop()
	t.statsd.Close()
	if t.dataStreams != nil {
		t.dataStreams.Stop()
	}
	appsec.Stop()
	remoteconfig.Stop()
	// Close log file last to account for any logs from the above calls
	if t.logFile != nil {
		t.logFile.Close()
	}
}

// Inject uses the configured or default TextMap Propagator.
func (t *tracer) Inject(ctx ddtrace.SpanContext, carrier interface{}) error {
	if !t.config.enabled.current {
		return nil
	}

	if t.config.tracingAsTransport {
		// in tracing as transport mode, only propagate when there is an upstream appsec event
		// TODO: replace with _dd.p.ts in the next iteration standardizing this for other products, comparing enabled products in `t.config` with their corresponding `_dd.p.ts` bitfields
		if ctx, ok := ctx.(*spanContext); ok && ctx.trace != nil && ctx.trace.propagatingTag("_dd.p.appsec") != "1" {
			return nil
		}
	}

	t.updateSampling(ctx)
	return t.config.propagator.Inject(ctx, carrier)
}

// updateSampling runs trace sampling rules on the context, since properties like resource / tags
// could change and impact the result of sampling. This must be done once before context is propagated.
func (t *tracer) updateSampling(ctx ddtrace.SpanContext) {
	sctx, ok := ctx.(*spanContext)
	if sctx == nil || !ok {
		return
	}
	// without this check some mock spans tests fail
	if t.rulesSampling == nil || sctx.trace == nil || sctx.trace.root == nil {
		return
	}
	// want to avoid locking the entire trace from a span for long.
	// if SampleTrace successfully samples the trace,
	// it will lock the span and the trace mutexes in span.setSamplingPriorityLocked
	// and trace.setSamplingPriority respectively, so we can't rely on those mutexes.
	if sctx.trace.isLocked() {
		// trace sampling decision already taken and locked, no re-sampling shall occur
		return
	}

	// the span was sampled with ManualKeep rules shouldn't override
	if sctx.trace.propagatingTag(keyDecisionMaker) == "-4" {
		return
	}
	// if sampling was successful, need to lock the trace to prevent further re-sampling
	if t.rulesSampling.SampleTrace(sctx.trace.root) {
		sctx.trace.setLocked(true)
	}
}

// Extract uses the configured or default TextMap Propagator.
func (t *tracer) Extract(carrier interface{}) (ddtrace.SpanContext, error) {
	if !t.config.enabled.current {
		return internal.NoopSpanContext{}, nil
	}
	ctx, err := t.config.propagator.Extract(carrier)
	if t.config.tracingAsTransport {
		// in tracing as transport mode, reset upstream sampling decision to make sure we keep 1 trace/minute
		// TODO: replace with _dd.p.ts in the next iteration standardizing this for other products, comparing enabled products in `t.config` with their corresponding `_dd.p.ts` bitfields
		if ctx, ok := ctx.(*spanContext); ok && ctx.trace.propagatingTag("_dd.p.appsec") != "1" {
			ctx.trace.priority = nil
		}
	}
	return ctx, err
}

// sampleRateMetricKey is the metric key holding the applied sample rate. Has to be the same as the Agent.
const sampleRateMetricKey = "_sample_rate"

// Sample samples a span with the internal sampler.
func (t *tracer) sample(span *span) {
	if _, ok := span.context.SamplingPriority(); ok {
		// sampling decision was already made
		return
	}
	sampler := t.config.sampler
	if !sampler.Sample(span) {
		span.context.trace.drop()
		span.context.trace.setSamplingPriority(ext.PriorityAutoReject, samplernames.RuleRate)
		return
	}
	if rs, ok := sampler.(RateSampler); ok && rs.Rate() < 1 {
		span.setMetric(sampleRateMetricKey, rs.Rate())
	}
	if t.rulesSampling.SampleTraceGlobalRate(span) {
		return
	}
	if t.rulesSampling.SampleTrace(span) {
		return
	}
	t.prioritySampling.apply(span)
}

func startExecutionTracerTask(ctx gocontext.Context, span *span) (gocontext.Context, func()) {
	if !rt.IsEnabled() {
		return ctx, func() {}
	}
	span.goExecTraced = true
	// Task name is the resource (operationName) of the span, e.g.
	// "POST /foo/bar" (http) or "/foo/pkg.Method" (grpc).
	taskName := span.Resource
	// If the resource could contain PII (e.g. SQL query that's not using bind
	// arguments), play it safe and just use the span type as the taskName,
	// e.g. "sql".
	if !spanResourcePIISafe(span) {
		taskName = span.Type
	}
	end := noopTaskEnd
	if !globalinternal.IsExecutionTraced(ctx) {
		var task *rt.Task
		ctx, task = rt.NewTask(ctx, taskName)
		end = task.End
	} else {
		// We only want to skip task creation for this particular span,
		// not necessarily for child spans which can come from different
		// integrations. So update this context to be "not" execution
		// traced so that derived contexts used by child spans don't get
		// skipped.
		ctx = globalinternal.WithExecutionNotTraced(ctx)
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], span.SpanID)
	// TODO: can we make string(b[:]) not allocate? e.g. with unsafe
	// shenanigans? rt.Log won't retain the message string, though perhaps
	// we can't assume that will always be the case.
	rt.Log(ctx, "datadog.uint64_span_id", string(b[:]))
	return ctx, end
}

func noopTaskEnd() {}

func (t *tracer) hostname() string {
	if !t.config.enableHostnameDetection {
		return ""
	}
	return hostname.Get()
}
