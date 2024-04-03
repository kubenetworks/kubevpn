// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"strconv"
	"strings"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/ext"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/globalconfig"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/samplernames"
)

// SQLCommentInjectionMode represents the mode of SQL comment injection.
//
// Deprecated: Use DBMPropagationMode instead.
type SQLCommentInjectionMode DBMPropagationMode

const (
	// SQLInjectionUndefined represents the comment injection mode is not set. This is the same as SQLInjectionDisabled.
	SQLInjectionUndefined SQLCommentInjectionMode = SQLCommentInjectionMode(DBMPropagationModeUndefined)
	// SQLInjectionDisabled represents the comment injection mode where all injection is disabled.
	SQLInjectionDisabled SQLCommentInjectionMode = SQLCommentInjectionMode(DBMPropagationModeDisabled)
	// SQLInjectionModeService represents the comment injection mode where only service tags (name, env, version) are injected.
	SQLInjectionModeService SQLCommentInjectionMode = SQLCommentInjectionMode(DBMPropagationModeService)
	// SQLInjectionModeFull represents the comment injection mode where both service tags and tracing tags. Tracing tags include span id, trace id and sampling priority.
	SQLInjectionModeFull SQLCommentInjectionMode = SQLCommentInjectionMode(DBMPropagationModeFull)
)

// DBMPropagationMode represents the mode of dbm propagation.
//
// Note that enabling sql comment propagation results in potentially confidential data (service names)
// being stored in the databases which can then be accessed by other 3rd parties that have been granted
// access to the database.
type DBMPropagationMode string

const (
	// DBMPropagationModeUndefined represents the dbm propagation mode not being set. This is the same as DBMPropagationModeDisabled.
	DBMPropagationModeUndefined DBMPropagationMode = ""
	// DBMPropagationModeDisabled represents the dbm propagation mode where all propagation is disabled.
	DBMPropagationModeDisabled DBMPropagationMode = "disabled"
	// DBMPropagationModeService represents the dbm propagation mode where only service tags (name, env, version) are propagated to dbm.
	DBMPropagationModeService DBMPropagationMode = "service"
	// DBMPropagationModeFull represents the dbm propagation mode where both service tags and tracing tags are propagated. Tracing tags include span id, trace id and the sampled flag.
	DBMPropagationModeFull DBMPropagationMode = "full"
)

// Key names for SQL comment tags.
const (
	sqlCommentTraceParent   = "traceparent"
	sqlCommentParentService = "ddps"
	sqlCommentDBService     = "dddbs"
	sqlCommentParentVersion = "ddpv"
	sqlCommentEnv           = "dde"
	// These keys are for the database we are connecting to, instead of the service we are running in.
	// "Peer" is the OpenTelemetry nomenclature for "thing I am talking to"
	sqlCommentPeerHostname = "ddh"
	sqlCommentPeerDBName   = "dddb"
)

// Current trace context version (see https://www.w3.org/TR/trace-context/#version)
const w3cContextVersion = "00"

// SQLCommentCarrier is a carrier implementation that injects a span context in a SQL query in the form
// of a sqlcommenter formatted comment prepended to the original query text.
// See https://google.github.io/sqlcommenter/spec/ for more details.
type SQLCommentCarrier struct {
	Query          string
	Mode           DBMPropagationMode
	DBServiceName  string
	SpanID         uint64
	PeerDBHostname string
	PeerDBName     string
}

// Inject injects a span context in the carrier's Query field as a comment.
func (c *SQLCommentCarrier) Inject(spanCtx ddtrace.SpanContext) error {
	c.SpanID = generateSpanID(now())
	tags := make(map[string]string)
	switch c.Mode {
	case DBMPropagationModeUndefined:
		fallthrough
	case DBMPropagationModeDisabled:
		return nil
	case DBMPropagationModeFull:
		var (
			sampled int64
			traceID uint64
		)
		if ctx, ok := spanCtx.(*spanContext); ok {
			if sp, ok := ctx.SamplingPriority(); ok && sp > 0 {
				sampled = 1
			}
			traceID = ctx.TraceID()
		}
		if traceID == 0 { // check if this is a root span
			traceID = c.SpanID
		}
		tags[sqlCommentTraceParent] = encodeTraceParent(traceID, c.SpanID, sampled)
		fallthrough
	case DBMPropagationModeService:
		if ctx, ok := spanCtx.(*spanContext); ok {
			if e, ok := ctx.meta(ext.Environment); ok && e != "" {
				tags[sqlCommentEnv] = e
			}
			if v, ok := ctx.meta(ext.Version); ok && v != "" {
				tags[sqlCommentParentVersion] = v
			}
			if c.PeerDBName != "" {
				tags[sqlCommentPeerDBName] = c.PeerDBName
			}
			if c.PeerDBHostname != "" {
				tags[sqlCommentPeerHostname] = c.PeerDBHostname
			}
		}
		if globalconfig.ServiceName() != "" {
			tags[sqlCommentParentService] = globalconfig.ServiceName()
		}
		tags[sqlCommentDBService] = c.DBServiceName
	}
	c.Query = commentQuery(c.Query, tags)
	return nil
}

// encodeTraceParent encodes trace parent as per the w3c trace context spec (https://www.w3.org/TR/trace-context/#version).
func encodeTraceParent(traceID uint64, spanID uint64, sampled int64) string {
	var b strings.Builder
	// traceparent has a fixed length of 55:
	// 2 bytes for the version, 32 for the trace id, 16 for the span id, 2 for the sampled flag and 3 for separators
	b.Grow(55)
	b.WriteString(w3cContextVersion)
	b.WriteRune('-')
	tid := strconv.FormatUint(traceID, 16)
	for i := 0; i < 32-len(tid); i++ {
		b.WriteRune('0')
	}
	b.WriteString(tid)
	b.WriteRune('-')
	sid := strconv.FormatUint(spanID, 16)
	for i := 0; i < 16-len(sid); i++ {
		b.WriteRune('0')
	}
	b.WriteString(sid)
	b.WriteRune('-')
	b.WriteRune('0')
	b.WriteString(strconv.FormatInt(sampled, 16))
	return b.String()
}

var (
	keyReplacer   = strings.NewReplacer(" ", "%20", "!", "%21", "#", "%23", "$", "%24", "%", "%25", "&", "%26", "'", "%27", "(", "%28", ")", "%29", "*", "%2A", "+", "%2B", ",", "%2C", "/", "%2F", ":", "%3A", ";", "%3B", "=", "%3D", "?", "%3F", "@", "%40", "[", "%5B", "]", "%5D")
	valueReplacer = strings.NewReplacer(" ", "%20", "!", "%21", "#", "%23", "$", "%24", "%", "%25", "&", "%26", "'", "%27", "(", "%28", ")", "%29", "*", "%2A", "+", "%2B", ",", "%2C", "/", "%2F", ":", "%3A", ";", "%3B", "=", "%3D", "?", "%3F", "@", "%40", "[", "%5B", "]", "%5D", "'", "\\'")
)

// commentQuery returns the given query with the tags from the SQLCommentCarrier applied to it as a
// prepended SQL comment. The format of the comment follows the sqlcommenter spec.
// See https://google.github.io/sqlcommenter/spec/ for more details.
func commentQuery(query string, tags map[string]string) string {
	if len(tags) == 0 {
		return ""
	}
	var b strings.Builder
	// the sqlcommenter specification dictates that tags should be sorted. Since we know all injected keys,
	// we skip a sorting operation by specifying the order of keys statically
	orderedKeys := []string{sqlCommentDBService, sqlCommentEnv, sqlCommentParentService, sqlCommentParentVersion, sqlCommentTraceParent, sqlCommentPeerHostname, sqlCommentPeerDBName}
	first := true
	for _, k := range orderedKeys {
		if v, ok := tags[k]; ok {
			// we need to URL-encode both keys and values and escape single quotes in values
			// https://google.github.io/sqlcommenter/spec/
			key := keyReplacer.Replace(k)
			val := valueReplacer.Replace(v)
			if first {
				b.WriteString("/*")
			} else {
				b.WriteRune(',')
			}
			b.WriteString(key)
			b.WriteRune('=')
			b.WriteRune('\'')
			b.WriteString(val)
			b.WriteRune('\'')
			first = false
		}
	}
	if b.Len() == 0 {
		return query
	}
	b.WriteString("*/")
	if query == "" {
		return b.String()
	}
	log.Debug("Injected sql comment: %s", b.String())
	b.WriteRune(' ')
	b.WriteString(query)
	return b.String()
}

// Extract parses for key value attributes in a sql query injected with trace information in order to build a span context
func (c *SQLCommentCarrier) Extract() (ddtrace.SpanContext, error) {
	var ctx *spanContext
	// There may be multiple comments within the sql query, so we must identify which one contains trace information.
	// We look at each comment until we find one that contains a traceparent
	if traceComment, found := findTraceComment(c.Query); found {
		var err error
		if ctx, err = spanContextFromTraceComment(traceComment); err != nil {
			return nil, err
		}
	} else {
		return nil, ErrSpanContextNotFound
	}
	if ctx.traceID.Empty() || ctx.spanID == 0 {
		return nil, ErrSpanContextNotFound
	}
	return ctx, nil
}

// spanContextFromTraceComment looks for specific kv pairs in a comment containing trace information.
// It returns a span context with the appropriate attributes
func spanContextFromTraceComment(c string) (*spanContext, error) {
	var ctx spanContext
	kvs := strings.Split(c, ",")
	for _, unparsedKV := range kvs {
		splitKV := strings.Split(unparsedKV, "=")
		if len(splitKV) != 2 {
			return nil, ErrSpanContextCorrupted
		}
		key := splitKV[0]
		value := strings.Trim(splitKV[1], "'")
		switch key {
		case sqlCommentTraceParent:
			traceIDLower, traceIDUpper, spanID, sampled, err := decodeTraceParent(value)
			if err != nil {
				return nil, err
			}
			ctx.traceID.SetLower(traceIDLower)
			ctx.traceID.SetUpper(traceIDUpper)
			ctx.spanID = spanID
			ctx.setSamplingPriority(sampled, samplernames.Unknown)
		default:
		}
	}
	return &ctx, nil
}

// decodeTraceParent decodes trace parent as per the w3c trace context spec (https://www.w3.org/TR/trace-context/#version).
// this also supports decoding traceparents from open telemetry sql comments which are 128 bit
func decodeTraceParent(traceParent string) (traceIDLower uint64, traceIDUpper uint64, spanID uint64, sampled int, err error) {
	if len(traceParent) < 55 {
		return 0, 0, 0, 0, ErrSpanContextCorrupted
	}
	version := traceParent[0:2]
	switch version {
	case w3cContextVersion:
		if traceIDUpper, err = strconv.ParseUint(traceParent[3:19], 16, 64); err != nil {
			return 0, 0, 0, 0, ErrSpanContextCorrupted
		}
		if traceIDLower, err = strconv.ParseUint(traceParent[19:35], 16, 64); err != nil {
			return 0, 0, 0, 0, ErrSpanContextCorrupted
		}
		if spanID, err = strconv.ParseUint(traceParent[36:52], 16, 64); err != nil {
			return 0, 0, 0, 0, ErrSpanContextCorrupted
		}
		if sampled, err = strconv.Atoi(traceParent[53:55]); err != nil {
			return 0, 0, 0, 0, ErrSpanContextCorrupted
		}
	default:
	}
	return traceIDLower, traceIDUpper, spanID, sampled, err
}

// findTraceComment looks for a sql comment that contains trace information by looking for the keyword traceparent
func findTraceComment(query string) (traceComment string, found bool) {
	startIndex := -1
	containsTrace := false
	keyLength := len(sqlCommentTraceParent)
	qLength := len(query)
	for i := 0; i < qLength-1; {
		if query[i] == '/' && query[i+1] == '*' {
			// look for leading /*
			startIndex = i
			i += 2
			containsTrace = false
		} else if query[i] == '*' && query[i+1] == '/' {
			// look for closing */
			if startIndex == -1 {
				// malformed comment, it did not have a leading /*
				return "", false
			}
			if !containsTrace {
				// ignore this comment, it was not a trace comment
				startIndex = -1
				i += 2
			} else {
				// do not return the query with the leading /* or trailing */
				return query[startIndex+2 : i], true
			}
		} else if !containsTrace && i+keyLength < qLength && query[i:i+keyLength] == sqlCommentTraceParent {
			// look for occurrence of keyword in the query if not yet found and make sure we don't go out of range
			containsTrace = true
			i += keyLength
		} else {
			i++
		}
	}
	return "", false
}
