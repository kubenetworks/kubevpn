// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/ext"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/internal"
	ginternal "gopkg.in/DataDog/dd-trace-go.v1/internal"
	sharedinternal "gopkg.in/DataDog/dd-trace-go.v1/internal"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/samplernames"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/telemetry"
)

var _ ddtrace.SpanContext = (*spanContext)(nil)

type traceID [16]byte // traceID in big endian, i.e. <upper><lower>

var emptyTraceID traceID

func (t *traceID) HexEncoded() string {
	return hex.EncodeToString(t[:])
}

func (t *traceID) Lower() uint64 {
	return binary.BigEndian.Uint64(t[8:])
}

func (t *traceID) Upper() uint64 {
	return binary.BigEndian.Uint64(t[:8])
}

func (t *traceID) SetLower(i uint64) {
	binary.BigEndian.PutUint64(t[8:], i)
}

func (t *traceID) SetUpper(i uint64) {
	binary.BigEndian.PutUint64(t[:8], i)
}

func (t *traceID) SetUpperFromHex(s string) error {
	u, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return fmt.Errorf("malformed %q: %s", s, err)
	}
	t.SetUpper(u)
	return nil
}

func (t *traceID) Empty() bool {
	return *t == emptyTraceID
}

func (t *traceID) HasUpper() bool {
	//TODO: in go 1.20 we can simplify this
	for _, b := range t[:8] {
		if b != 0 {
			return true
		}
	}
	return false
}

func (t *traceID) UpperHex() string {
	return hex.EncodeToString(t[:8])
}

// SpanContext represents a span state that can propagate to descendant spans
// and across process boundaries. It contains all the information needed to
// spawn a direct descendant of the span that it belongs to. It can be used
// to create distributed tracing by propagating it using the provided interfaces.
type spanContext struct {
	updated bool // updated is tracking changes for priority / origin / x-datadog-tags

	// the below group should propagate only locally

	trace  *trace // reference to the trace that this span belongs too
	span   *span  // reference to the span that hosts this context
	errors int32  // number of spans with errors in this trace

	// the below group should propagate cross-process

	traceID traceID
	spanID  uint64

	mu         sync.RWMutex // guards below fields
	baggage    map[string]string
	hasBaggage uint32 // atomic int for quick checking presence of baggage. 0 indicates no baggage, otherwise baggage exists.
	origin     string // e.g. "synthetics"
}

// newSpanContext creates a new SpanContext to serve as context for the given
// span. If the provided parent is not nil, the context will inherit the trace,
// baggage and other values from it. This method also pushes the span into the
// new context's trace and as a result, it should not be called multiple times
// for the same span.
func newSpanContext(span *span, parent *spanContext) *spanContext {
	context := &spanContext{
		spanID: span.SpanID,
		span:   span,
	}
	context.traceID.SetLower(span.TraceID)
	if parent != nil {
		context.traceID.SetUpper(parent.traceID.Upper())
		context.trace = parent.trace
		context.origin = parent.origin
		context.errors = parent.errors
		parent.ForeachBaggageItem(func(k, v string) bool {
			context.setBaggageItem(k, v)
			return true
		})
	} else if sharedinternal.BoolEnv("DD_TRACE_128_BIT_TRACEID_GENERATION_ENABLED", true) {
		// add 128 bit trace id, if enabled, formatted as big-endian:
		// <32-bit unix seconds> <32 bits of zero> <64 random bits>
		id128 := time.Duration(span.Start) / time.Second
		// casting from int64 -> uint32 should be safe since the start time won't be
		// negative, and the seconds should fit within 32-bits for the foreseeable future.
		// (We only want 32 bits of time, then the rest is zero)
		tUp := uint64(uint32(id128)) << 32 // We need the time at the upper 32 bits of the uint
		context.traceID.SetUpper(tUp)
	}
	if context.trace == nil {
		context.trace = newTrace()
	}
	if context.trace.root == nil {
		// first span in the trace can safely be assumed to be the root
		context.trace.root = span
	}
	// put span in context's trace
	context.trace.push(span)
	// setting context.updated to false here is necessary to distinguish
	// between initializing properties of the span (priority)
	// and updating them after extracting context through propagators
	context.updated = false
	return context
}

// SpanID implements ddtrace.SpanContext.
func (c *spanContext) SpanID() uint64 { return c.spanID }

// TraceID implements ddtrace.SpanContext.
func (c *spanContext) TraceID() uint64 { return c.traceID.Lower() }

// TraceID128 implements ddtrace.SpanContextW3C.
func (c *spanContext) TraceID128() string {
	if c == nil {
		return ""
	}
	return c.traceID.HexEncoded()
}

// TraceID128Bytes implements ddtrace.SpanContextW3C.
func (c *spanContext) TraceID128Bytes() [16]byte {
	return c.traceID
}

// ForeachBaggageItem implements ddtrace.SpanContext.
func (c *spanContext) ForeachBaggageItem(handler func(k, v string) bool) {
	if atomic.LoadUint32(&c.hasBaggage) == 0 {
		return
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for k, v := range c.baggage {
		if !handler(k, v) {
			break
		}
	}
}

func (c *spanContext) setSamplingPriority(p int, sampler samplernames.SamplerName) {
	if c.trace == nil {
		c.trace = newTrace()
	}
	if c.trace.setSamplingPriority(p, sampler) {
		// the trace's sampling priority was updated: mark this as updated
		c.updated = true
	}
}

func (c *spanContext) SamplingPriority() (p int, ok bool) {
	if c.trace == nil {
		return 0, false
	}
	return c.trace.samplingPriority()
}

func (c *spanContext) setBaggageItem(key, val string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.baggage == nil {
		atomic.StoreUint32(&c.hasBaggage, 1)
		c.baggage = make(map[string]string, 1)
	}
	c.baggage[key] = val
}

func (c *spanContext) baggageItem(key string) string {
	if atomic.LoadUint32(&c.hasBaggage) == 0 {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baggage[key]
}

func (c *spanContext) meta(key string) (val string, ok bool) {
	c.span.RLock()
	defer c.span.RUnlock()
	val, ok = c.span.Meta[key]
	return val, ok
}

// finish marks this span as finished in the trace.
func (c *spanContext) finish() { c.trace.finishedOne(c.span) }

// samplingDecision is the decision to send a trace to the agent or not.
type samplingDecision uint32

const (
	// decisionNone is the default state of a trace.
	// If no decision is made about the trace, the trace won't be sent to the agent.
	decisionNone samplingDecision = iota
	// decisionDrop prevents the trace from being sent to the agent.
	decisionDrop
	// decisionKeep ensures the trace will be sent to the agent.
	decisionKeep
)

// trace contains shared context information about a trace, such as sampling
// priority, the root reference and a buffer of the spans which are part of the
// trace, if these exist.
type trace struct {
	mu               sync.RWMutex      // guards below fields
	spans            []*span           // all the spans that are part of this trace
	tags             map[string]string // trace level tags
	propagatingTags  map[string]string // trace level tags that will be propagated across service boundaries
	finished         int               // the number of finished spans
	full             bool              // signifies that the span buffer is full
	priority         *float64          // sampling priority
	locked           bool              // specifies if the sampling priority can be altered
	samplingDecision samplingDecision  // samplingDecision indicates whether to send the trace to the agent.

	// root specifies the root of the trace, if known; it is nil when a span
	// context is extracted from a carrier, at which point there are no spans in
	// the trace yet.
	root *span
}

var (
	// traceStartSize is the initial size of our trace buffer,
	// by default we allocate for a handful of spans within the trace,
	// reasonable as span is actually way bigger, and avoids re-allocating
	// over and over. Could be fine-tuned at runtime.
	traceStartSize = 10
	// traceMaxSize is the maximum number of spans we keep in memory for a
	// single trace. This is to avoid memory leaks. If more spans than this
	// are added to a trace, then the trace is dropped and the spans are
	// discarded. Adding additional spans after a trace is dropped does
	// nothing.
	traceMaxSize = int(1e5)
)

// newTrace creates a new trace using the given callback which will be called
// upon completion of the trace.
func newTrace() *trace {
	return &trace{spans: make([]*span, 0, traceStartSize)}
}

func (t *trace) samplingPriorityLocked() (p int, ok bool) {
	if t.priority == nil {
		return 0, false
	}
	return int(*t.priority), true
}

func (t *trace) samplingPriority() (p int, ok bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.samplingPriorityLocked()
}

// setSamplingPriority sets the sampling priority and returns true if it was modified.
func (t *trace) setSamplingPriority(p int, sampler samplernames.SamplerName) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.setSamplingPriorityLocked(p, sampler)
}

func (t *trace) keep() {
	atomic.CompareAndSwapUint32((*uint32)(&t.samplingDecision), uint32(decisionNone), uint32(decisionKeep))
}

func (t *trace) drop() {
	atomic.CompareAndSwapUint32((*uint32)(&t.samplingDecision), uint32(decisionNone), uint32(decisionDrop))
}

func (t *trace) setTag(key, value string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.setTagLocked(key, value)
}

func (t *trace) setTagLocked(key, value string) {
	if t.tags == nil {
		t.tags = make(map[string]string, 1)
	}
	t.tags[key] = value
}

func (t *trace) setSamplingPriorityLocked(p int, sampler samplernames.SamplerName) bool {
	if t.locked {
		return false
	}

	updatedPriority := t.priority == nil || *t.priority != float64(p)

	if t.priority == nil {
		t.priority = new(float64)
	}
	*t.priority = float64(p)
	_, ok := t.propagatingTags[keyDecisionMaker]
	if p > 0 && !ok && sampler != samplernames.Unknown {
		// We have a positive priority and the sampling mechanism isn't set.
		// Send nothing when sampler is `Unknown` for RFC compliance.
		t.setPropagatingTagLocked(keyDecisionMaker, "-"+strconv.Itoa(int(sampler)))
	}
	if p <= 0 && ok {
		delete(t.propagatingTags, keyDecisionMaker)
	}

	return updatedPriority
}

func (t *trace) isLocked() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.locked
}

func (t *trace) setLocked(locked bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.locked = locked
}

// push pushes a new span into the trace. If the buffer is full, it returns
// a errBufferFull error.
func (t *trace) push(sp *span) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.full {
		return
	}
	tr, haveTracer := internal.GetGlobalTracer().(*tracer)
	if len(t.spans) >= traceMaxSize {
		// capacity is reached, we will not be able to complete this trace.
		t.full = true
		t.spans = nil // GC
		log.Error("trace buffer full (%d), dropping trace", traceMaxSize)
		if haveTracer {
			atomic.AddUint32(&tr.tracesDropped, 1)
		}
		return
	}
	if v, ok := sp.Metrics[keySamplingPriority]; ok {
		t.setSamplingPriorityLocked(int(v), samplernames.Unknown)
	}
	t.spans = append(t.spans, sp)
	if haveTracer {
		atomic.AddUint32(&tr.spansStarted, 1)
	}
}

// setTraceTags sets all "trace level" tags on the provided span
// t must already be locked.
func (t *trace) setTraceTags(s *span, tr *tracer) {
	for k, v := range t.tags {
		s.setMeta(k, v)
	}
	for k, v := range t.propagatingTags {
		s.setMeta(k, v)
	}
	for k, v := range ginternal.GetTracerGitMetadataTags() {
		s.setMeta(k, v)
	}
	if s.context != nil && s.context.traceID.HasUpper() {
		s.setMeta(keyTraceID128, s.context.traceID.UpperHex())
	}
	if hn := tr.hostname(); hn != "" {
		s.setMeta(keyTracerHostname, hn)
	}
}

// finishedOne acknowledges that another span in the trace has finished, and checks
// if the trace is complete, in which case it calls the onFinish function. It uses
// the given priority, if non-nil, to mark the root span. This also will trigger a partial flush
// if enabled and the total number of finished spans is greater than or equal to the partial flush limit.
// The provided span must be locked.
func (t *trace) finishedOne(s *span) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s.finished = true
	if t.full {
		// capacity has been reached, the buffer is no longer tracking
		// all the spans in the trace, so the below conditions will not
		// be accurate and would trigger a pre-mature flush, exposing us
		// to a race condition where spans can be modified while flushing.
		//
		// TODO(partialFlush): should we do a partial flush in this scenario?
		return
	}
	t.finished++
	tr, ok := internal.GetGlobalTracer().(*tracer)
	if !ok {
		return
	}
	setPeerService(s, tr.config)

	// attach the _dd.base_service tag only when the globally configured service name is different from the
	// span service name.
	if s.Service != "" && !strings.EqualFold(s.Service, tr.config.serviceName) {
		s.Meta[keyBaseService] = tr.config.serviceName
	}
	if s == t.root && t.priority != nil {
		// after the root has finished we lock down the priority;
		// we won't be able to make changes to a span after finishing
		// without causing a race condition.
		t.root.setMetric(keySamplingPriority, *t.priority)
		t.locked = true
	}
	if len(t.spans) > 0 && s == t.spans[0] {
		// first span in chunk finished, lock down the tags
		//
		// TODO(barbayar): make sure this doesn't happen in vain when switching to
		// the new wire format. We won't need to set the tags on the first span
		// in the chunk there.
		t.setTraceTags(s, tr)
	}

	if len(t.spans) == t.finished { // perform a full flush of all spans
		t.finishChunk(tr, &chunk{
			spans:    t.spans,
			willSend: decisionKeep == samplingDecision(atomic.LoadUint32((*uint32)(&t.samplingDecision))),
		})
		t.spans = nil
		return
	}

	doPartialFlush := tr.config.partialFlushEnabled && t.finished >= tr.config.partialFlushMinSpans
	if !doPartialFlush {
		return // The trace hasn't completed and partial flushing will not occur
	}
	log.Debug("Partial flush triggered with %d finished spans", t.finished)
	telemetry.GlobalClient.Count(telemetry.NamespaceTracers, "trace_partial_flush.count", 1, []string{"reason:large_trace"}, true)
	finishedSpans := make([]*span, 0, t.finished)
	leftoverSpans := make([]*span, 0, len(t.spans)-t.finished)
	for _, s2 := range t.spans {
		if s2.finished {
			finishedSpans = append(finishedSpans, s2)
		} else {
			leftoverSpans = append(leftoverSpans, s2)
		}
	}
	// TODO: (Support MetricKindDist) Re-enable these when we actually support `MetricKindDist`
	//telemetry.GlobalClient.Record(telemetry.NamespaceTracers, telemetry.MetricKindDist, "trace_partial_flush.spans_closed", float64(len(finishedSpans)), nil, true)
	//telemetry.GlobalClient.Record(telemetry.NamespaceTracers, telemetry.MetricKindDist, "trace_partial_flush.spans_remaining", float64(len(leftoverSpans)), nil, true)
	finishedSpans[0].setMetric(keySamplingPriority, *t.priority)
	if s != t.spans[0] {
		// Make sure the first span in the chunk has the trace-level tags
		t.setTraceTags(finishedSpans[0], tr)
	}
	t.finishChunk(tr, &chunk{
		spans:    finishedSpans,
		willSend: decisionKeep == samplingDecision(atomic.LoadUint32((*uint32)(&t.samplingDecision))),
	})
	t.spans = leftoverSpans
}

func (t *trace) finishChunk(tr *tracer, ch *chunk) {
	atomic.AddUint32(&tr.spansFinished, uint32(len(ch.spans)))
	tr.pushChunk(ch)
	t.finished = 0 // important, because a buffer can be used for several flushes
}

// setPeerService sets the peer.service, _dd.peer.service.source, and _dd.peer.service.remapped_from
// tags as applicable for the given span.
func setPeerService(s *span, cfg *config) {
	if _, ok := s.Meta[ext.PeerService]; ok { // peer.service already set on the span
		s.setMeta(keyPeerServiceSource, ext.PeerService)
	} else { // no peer.service currently set
		spanKind := s.Meta[ext.SpanKind]
		isOutboundRequest := spanKind == ext.SpanKindClient || spanKind == ext.SpanKindProducer
		shouldSetDefaultPeerService := isOutboundRequest && cfg.peerServiceDefaultsEnabled
		if !shouldSetDefaultPeerService {
			return
		}
		source := setPeerServiceFromSource(s)
		if source == "" {
			log.Debug("No source tag value could be found for span %q, peer.service not set", s.Name)
			return
		}
		s.setMeta(keyPeerServiceSource, source)
	}
	// Overwrite existing peer.service value if remapped by the user
	ps := s.Meta[ext.PeerService]
	if to, ok := cfg.peerServiceMappings[ps]; ok {
		s.setMeta(keyPeerServiceRemappedFrom, ps)
		s.setMeta(ext.PeerService, to)
	}
}

// setPeerServiceFromSource sets peer.service from the sources determined
// by the tags on the span. It returns the source tag name that it used for
// the peer.service value, or the empty string if no valid source tag was available.
func setPeerServiceFromSource(s *span) string {
	has := func(tag string) bool {
		_, ok := s.Meta[tag]
		return ok
	}
	var sources []string
	useTargetHost := true
	switch {
	// order of the cases and their sources matters here. These are in priority order (highest to lowest)
	case has("aws_service"):
		sources = []string{
			"queuename",
			"topicname",
			"streamname",
			"tablename",
			"bucketname",
		}
	case s.Meta[ext.DBSystem] == ext.DBSystemCassandra:
		sources = []string{
			ext.CassandraContactPoints,
		}
		useTargetHost = false
	case has(ext.DBSystem):
		sources = []string{
			ext.DBName,
			ext.DBInstance,
		}
	case has(ext.MessagingSystem):
		sources = []string{
			ext.KafkaBootstrapServers,
		}
	case has(ext.RPCSystem):
		sources = []string{
			ext.RPCService,
		}
	}
	// network destination tags will be used as fallback unless there are higher priority sources already set.
	if useTargetHost {
		sources = append(sources, []string{
			ext.NetworkDestinationName,
			ext.PeerHostname,
			ext.TargetHost,
		}...)
	}
	for _, source := range sources {
		if val, ok := s.Meta[source]; ok {
			s.setMeta(ext.PeerService, val)
			return source
		}
	}
	return ""
}
