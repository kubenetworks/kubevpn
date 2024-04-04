// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/ext"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/samplernames"

	"golang.org/x/time/rate"
)

// rulesSampler holds instances of trace sampler and single span sampler, that are configured with the given set of rules.
type rulesSampler struct {
	// traceRulesSampler samples trace spans based on a user-defined set of rules and might impact sampling decision of the trace.
	traces *traceRulesSampler

	// singleSpanRulesSampler samples individual spans based on a separate user-defined set of rules and
	// cannot impact the trace sampling decision.
	spans *singleSpanRulesSampler
}

// newRulesSampler configures a *rulesSampler instance using the given set of rules.
// Rules are split between trace and single span sampling rules according to their type.
// Such rules are user-defined through environment variable or WithSamplingRules option.
// Invalid rules or environment variable values are tolerated, by logging warnings and then ignoring them.
func newRulesSampler(traceRules, spanRules []SamplingRule, traceSampleRate float64) *rulesSampler {
	return &rulesSampler{
		traces: newTraceRulesSampler(traceRules, traceSampleRate),
		spans:  newSingleSpanRulesSampler(spanRules),
	}
}

func (r *rulesSampler) SampleTrace(s *span) bool { return r.traces.sampleRules(s) }

func (r *rulesSampler) SampleTraceGlobalRate(s *span) bool { return r.traces.sampleGlobalRate(s) }

func (r *rulesSampler) SampleSpan(s *span) bool { return r.spans.apply(s) }

func (r *rulesSampler) HasSpanRules() bool { return r.spans.enabled() }

func (r *rulesSampler) TraceRateLimit() (float64, bool) { return r.traces.limit() }

// SamplingRule is used for applying sampling rates to spans that match
// the service name, operation name or both.
// For basic usage, consider using the helper functions ServiceRule, NameRule, etc.
type SamplingRule struct {
	// Service specifies the regex pattern that a span service name must match.
	Service *regexp.Regexp

	// Name specifies the regex pattern that a span operation name must match.
	Name *regexp.Regexp

	// Rate specifies the sampling rate that should be applied to spans that match
	// service and/or name of the rule.
	Rate float64

	// MaxPerSecond specifies max number of spans per second that can be sampled per the rule.
	// If not specified, the default is no limit.
	MaxPerSecond float64

	// Resource specifies the regex pattern that a span resource must match.
	Resource *regexp.Regexp

	// Tags specifies the map of key-value patterns that span tags must match.
	Tags map[string]*regexp.Regexp

	ruleType SamplingRuleType
	limiter  *rateLimiter
}

// match returns true when the span's details match all the expected values in the rule.
func (sr *SamplingRule) match(s *span) bool {
	if sr.Service != nil && !sr.Service.MatchString(s.Service) {
		return false
	}
	if sr.Name != nil && !sr.Name.MatchString(s.Name) {
		return false
	}
	if sr.Resource != nil && !sr.Resource.MatchString(s.Resource) {
		return false
	}
	s.Lock()
	defer s.Unlock()
	if sr.Tags != nil && s.Meta != nil {
		for k, regex := range sr.Tags {
			v, ok := s.Meta[k]
			if !ok || !regex.MatchString(v) {
				return false
			}
		}
	}
	return true
}

// SamplingRuleType represents a type of sampling rule spans are matched against.
type SamplingRuleType int

const (
	SamplingRuleUndefined SamplingRuleType = 0

	// SamplingRuleTrace specifies a sampling rule that applies to the entire trace if any spans satisfy the criteria.
	// If a sampling rule is of type SamplingRuleTrace, such rule determines the sampling rate to apply
	// to trace spans. If a span matches that rule, it will impact the trace sampling decision.
	SamplingRuleTrace = iota

	// SamplingRuleSpan specifies a sampling rule that applies to a single span without affecting the entire trace.
	// If a sampling rule is of type SamplingRuleSingleSpan, such rule determines the sampling rate to apply
	// to individual spans. If a span matches a rule, it will NOT impact the trace sampling decision.
	// In the case that a trace is dropped and thus not sent to the Agent, spans kept on account
	// of matching SamplingRuleSingleSpan rules must be conveyed separately.
	SamplingRuleSpan
)

func (sr SamplingRuleType) String() string {
	switch sr {
	case SamplingRuleTrace:
		return "trace"
	case SamplingRuleSpan:
		return "span"
	default:
		return ""
	}
}

// ServiceRule returns a SamplingRule that applies the provided sampling rate
// to spans that match the service name provided.
func ServiceRule(service string, rate float64) SamplingRule {
	return SamplingRule{
		Service:  globMatch(service),
		ruleType: SamplingRuleTrace,
		Rate:     rate,
	}
}

// NameRule returns a SamplingRule that applies the provided sampling rate
// to spans that match the operation name provided.
func NameRule(name string, rate float64) SamplingRule {
	return SamplingRule{
		Name:     globMatch(name),
		ruleType: SamplingRuleTrace,
		Rate:     rate,
	}
}

// NameServiceRule returns a SamplingRule that applies the provided sampling rate
// to spans matching both the operation and service names provided.
func NameServiceRule(name string, service string, rate float64) SamplingRule {
	return SamplingRule{
		Service:  globMatch(service),
		Name:     globMatch(name),
		ruleType: SamplingRuleTrace,
		Rate:     rate,
	}
}

// RateRule returns a SamplingRule that applies the provided sampling rate to all spans.
func RateRule(rate float64) SamplingRule {
	return SamplingRule{
		Rate:     rate,
		ruleType: SamplingRuleTrace,
	}
}

// TagsResourceRule returns a SamplingRule that applies the provided sampling rate to traces with spans that match
// resource, name, service and tags provided.
func TagsResourceRule(tags map[string]*regexp.Regexp, resource, name, service string, rate float64) SamplingRule {
	return SamplingRule{
		Service:  globMatch(service),
		Name:     globMatch(name),
		Resource: globMatch(resource),
		Rate:     rate,
		Tags:     tags,
		ruleType: SamplingRuleTrace,
	}
}

// SpanTagsResourceRule returns a SamplingRule that applies the provided sampling rate to spans that match
// resource, name, service and tags provided. Values of the tags map are expected to be in glob format.
func SpanTagsResourceRule(tags map[string]string, resource, name, service string, rate float64) SamplingRule {
	globTags := make(map[string]*regexp.Regexp, len(tags))
	for k, v := range tags {
		if g := globMatch(v); g != nil {
			globTags[k] = g
		}
	}
	return SamplingRule{
		Service:  globMatch(service),
		Name:     globMatch(name),
		Resource: globMatch(resource),
		Rate:     rate,
		Tags:     globTags,
		ruleType: SamplingRuleSpan,
	}
}

// SpanNameServiceRule returns a SamplingRule of type SamplingRuleSpan that applies
// the provided sampling rate to all spans matching the operation and service name glob patterns provided.
// Operation and service fields must be valid glob patterns.
func SpanNameServiceRule(name, service string, rate float64) SamplingRule {
	return SamplingRule{
		Service:  globMatch(service),
		Name:     globMatch(name),
		Rate:     rate,
		ruleType: SamplingRuleSpan,
		limiter:  newSingleSpanRateLimiter(0),
	}
}

// SpanNameServiceMPSRule returns a SamplingRule of type SamplingRuleSpan that applies
// the provided sampling rate to all spans matching the operation and service name glob patterns
// up to the max number of spans per second that can be sampled.
// Operation and service fields must be valid glob patterns.
func SpanNameServiceMPSRule(name, service string, rate, limit float64) SamplingRule {
	return SamplingRule{
		Service:      globMatch(service),
		Name:         globMatch(name),
		MaxPerSecond: limit,
		Rate:         rate,
		ruleType:     SamplingRuleSpan,
		limiter:      newSingleSpanRateLimiter(limit),
	}
}

// traceRulesSampler allows a user-defined list of rules to apply to traces.
// These rules can match based on the span's Service, Name or both.
// When making a sampling decision, the rules are checked in order until
// a match is found.
// If a match is found, the rate from that rule is used.
// If no match is found, and the DD_TRACE_SAMPLE_RATE environment variable
// was set to a valid rate, that value is used.
// Otherwise, the rules sampler didn't apply to the span, and the decision
// is passed to the priority sampler.
//
// The rate is used to determine if the span should be sampled, but an upper
// limit can be defined using the DD_TRACE_RATE_LIMIT environment variable.
// Its value is the number of spans to sample per second.
// Spans that matched the rules but exceeded the rate limit are not sampled.
type traceRulesSampler struct {
	m          sync.RWMutex
	rules      []SamplingRule // the rules to match spans with
	globalRate float64        // a rate to apply when no rules match a span
	limiter    *rateLimiter   // used to limit the volume of spans sampled
}

// newTraceRulesSampler configures a *traceRulesSampler instance using the given set of rules.
// Invalid rules or environment variable values are tolerated, by logging warnings and then ignoring them.
func newTraceRulesSampler(rules []SamplingRule, traceSampleRate float64) *traceRulesSampler {
	return &traceRulesSampler{
		rules:      rules,
		globalRate: traceSampleRate,
		limiter:    newRateLimiter(),
	}
}

// globalSampleRate returns the sampling rate found in the DD_TRACE_SAMPLE_RATE environment variable.
// If it is invalid or not within the 0-1 range, NaN is returned.
func globalSampleRate() float64 {
	defaultRate := math.NaN()
	v := os.Getenv("DD_TRACE_SAMPLE_RATE")
	if v == "" {
		return defaultRate
	}
	r, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Warn("ignoring DD_TRACE_SAMPLE_RATE: error: %v", err)
		return defaultRate
	}
	if r >= 0.0 && r <= 1.0 {
		return r
	}
	log.Warn("ignoring DD_TRACE_SAMPLE_RATE: out of range %f", r)
	return defaultRate
}

func (rs *traceRulesSampler) enabled() bool {
	rs.m.RLock()
	defer rs.m.RUnlock()
	return len(rs.rules) > 0 || !math.IsNaN(rs.globalRate)
}

// setGlobalSampleRate sets the global sample rate to the given value.
// Returns whether the value was changed or not.
func (rs *traceRulesSampler) setGlobalSampleRate(rate float64) bool {
	if rate < 0.0 || rate > 1.0 {
		log.Warn("Ignoring trace sample rate %f: value out of range [0,1]", rate)
		return false
	}
	rs.m.Lock()
	defer rs.m.Unlock()
	if math.IsNaN(rs.globalRate) && math.IsNaN(rate) {
		// NaN is not considered equal to any number, including itself.
		// It should be compared with math.IsNaN
		return false
	}
	if rs.globalRate == rate {
		return false
	}
	rs.globalRate = rate
	return true
}

// sampleGlobalRate applies the global trace sampling rate to the span. If the rate is Nan,
// the function return false, then it returns false and the span is not
// modified.
func (rs *traceRulesSampler) sampleGlobalRate(span *span) bool {
	if !rs.enabled() {
		// short path when disabled
		return false
	}

	rs.m.RLock()
	rate := rs.globalRate
	rs.m.RUnlock()

	if math.IsNaN(rate) {
		return false
	}

	rs.applyRate(span, rate, time.Now())
	return true
}

// sampleRules uses the sampling rules to determine the sampling rate for the
// provided span. If the rules don't match, then it returns false and the span is not
// modified.
func (rs *traceRulesSampler) sampleRules(span *span) bool {
	if !rs.enabled() {
		// short path when disabled
		return false
	}

	var matched bool
	rs.m.RLock()
	rate := rs.globalRate
	rs.m.RUnlock()
	for _, rule := range rs.rules {
		if rule.match(span) {
			matched = true
			rate = rule.Rate
			break
		}
	}
	if !matched {
		// no matching rule or global rate, so we want to fall back
		// to priority sampling
		return false
	}

	rs.applyRate(span, rate, time.Now())
	return true
}

func (rs *traceRulesSampler) applyRate(span *span, rate float64, now time.Time) {
	span.SetTag(keyRulesSamplerAppliedRate, rate)
	if !sampledByRate(span.TraceID, rate) {
		span.setSamplingPriority(ext.PriorityUserReject, samplernames.RuleRate)
		return
	}

	sampled, rate := rs.limiter.allowOne(now)
	if sampled {
		span.setSamplingPriority(ext.PriorityUserKeep, samplernames.RuleRate)
	} else {
		span.setSamplingPriority(ext.PriorityUserReject, samplernames.RuleRate)
	}
	span.SetTag(keyRulesSamplerLimiterRate, rate)
}

// limit returns the rate limit set in the rules sampler, controlled by DD_TRACE_RATE_LIMIT, and
// true if rules sampling is enabled. If not present it returns math.NaN() and false.
func (rs *traceRulesSampler) limit() (float64, bool) {
	if rs.enabled() {
		return float64(rs.limiter.limiter.Limit()), true
	}
	return math.NaN(), false
}

// defaultRateLimit specifies the default trace rate limit used when DD_TRACE_RATE_LIMIT is not set.
const defaultRateLimit = 100.0

// newRateLimiter returns a rate limiter which restricts the number of traces sampled per second.
// The limit is DD_TRACE_RATE_LIMIT if set, `defaultRateLimit` otherwise.
func newRateLimiter() *rateLimiter {
	limit := defaultRateLimit
	v := os.Getenv("DD_TRACE_RATE_LIMIT")
	if v != "" {
		l, err := strconv.ParseFloat(v, 64)
		if err != nil {
			log.Warn("DD_TRACE_RATE_LIMIT invalid, using default value %f: %v", limit, err)
		} else if l < 0.0 {
			log.Warn("DD_TRACE_RATE_LIMIT negative, using default value %f", limit)
		} else {
			// override the default limit
			limit = l
		}
	}
	return &rateLimiter{
		limiter:  rate.NewLimiter(rate.Limit(limit), int(math.Ceil(limit))),
		prevTime: time.Now(),
	}
}

// singleSpanRulesSampler allows a user-defined list of rules to apply to spans
// to sample single spans.
// These rules match based on the span's Service and Name. If empty value is supplied
// to either Service or Name field, it will default to "*", allow all.
// When making a sampling decision, the rules are checked in order until
// a match is found.
// If a match is found, the rate from that rule is used.
// If no match is found, no changes or further sampling is applied to the spans.
// The rate is used to determine if the span should be sampled, but an upper
// limit can be defined using the max_per_second field when supplying the rule.
// If max_per_second is absent in the rule, the default is allow all.
// Its value is the max number of spans to sample per second.
// Spans that matched the rules but exceeded the rate limit are not sampled.
type singleSpanRulesSampler struct {
	rules []SamplingRule // the rules to match spans with
}

// newSingleSpanRulesSampler configures a *singleSpanRulesSampler instance using the given set of rules.
// Invalid rules or environment variable values are tolerated, by logging warnings and then ignoring them.
func newSingleSpanRulesSampler(rules []SamplingRule) *singleSpanRulesSampler {
	return &singleSpanRulesSampler{
		rules: rules,
	}
}

func (rs *singleSpanRulesSampler) enabled() bool {
	return len(rs.rules) > 0
}

// apply uses the sampling rules to determine the sampling rate for the
// provided span. If the rules don't match, then it returns false and the span is not
// modified.
func (rs *singleSpanRulesSampler) apply(span *span) bool {
	for _, rule := range rs.rules {
		if rule.match(span) {
			rate := rule.Rate
			span.setMetric(keyRulesSamplerAppliedRate, rate)
			if !sampledByRate(span.SpanID, rate) {
				return false
			}
			var sampled bool
			if rule.limiter != nil {
				sampled, rate = rule.limiter.allowOne(nowTime())
				if !sampled {
					return false
				}
			}
			span.setMetric(keySpanSamplingMechanism, float64(samplernames.SingleSpan))
			span.setMetric(keySingleSpanSamplingRuleRate, rate)
			if rule.MaxPerSecond != 0 {
				span.setMetric(keySingleSpanSamplingMPS, rule.MaxPerSecond)
			}
			return true
		}
	}
	return false
}

// rateLimiter is a wrapper on top of golang.org/x/time/rate which implements a rate limiter but also
// returns the effective rate of allowance.
type rateLimiter struct {
	limiter *rate.Limiter

	mu          sync.Mutex // guards below fields
	prevTime    time.Time  // time at which prevAllowed and prevSeen were set
	allowed     float64    // number of spans allowed in the current period
	seen        float64    // number of spans seen in the current period
	prevAllowed float64    // number of spans allowed in the previous period
	prevSeen    float64    // number of spans seen in the previous period
}

// allowOne returns the rate limiter's decision to allow the span to be sampled, and the
// effective rate at the time it is called. The effective rate is computed by averaging the rate
// for the previous second with the current rate
func (r *rateLimiter) allowOne(now time.Time) (bool, float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d := now.Sub(r.prevTime); d >= time.Second {
		// enough time has passed to reset the counters
		if d.Truncate(time.Second) == time.Second && r.seen > 0 {
			// exactly one second, so update prev
			r.prevAllowed = r.allowed
			r.prevSeen = r.seen
		} else {
			// more than one second, so reset previous rate
			r.prevAllowed = 0
			r.prevSeen = 0
		}
		r.prevTime = now
		r.allowed = 0
		r.seen = 0
	}

	r.seen++
	var sampled bool
	if r.limiter.AllowN(now, 1) {
		r.allowed++
		sampled = true
	}
	er := (r.prevAllowed + r.allowed) / (r.prevSeen + r.seen)
	return sampled, er
}

// newSingleSpanRateLimiter returns a rate limiter which restricts the number of single spans sampled per second.
// This defaults to infinite, allow all behaviour. The MaxPerSecond value of the rule may override the default.
func newSingleSpanRateLimiter(mps float64) *rateLimiter {
	limit := math.MaxFloat64
	if mps > 0 {
		limit = mps
	}
	return &rateLimiter{
		limiter:  rate.NewLimiter(rate.Limit(limit), int(math.Ceil(limit))),
		prevTime: time.Now(),
	}
}

// globMatch compiles pattern string into glob format, i.e. regular expressions with only '?'
// and '*' treated as regex metacharacters.
func globMatch(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	// escaping regex characters
	pattern = regexp.QuoteMeta(pattern)
	// replacing '?' and '*' with regex characters
	pattern = strings.Replace(pattern, "\\?", ".", -1)
	pattern = strings.Replace(pattern, "\\*", ".*", -1)
	// pattern must match an entire string
	return regexp.MustCompile(fmt.Sprintf("^%s$", pattern))
}

// samplingRulesFromEnv parses sampling rules from
// the DD_TRACE_SAMPLING_RULES, DD_TRACE_SAMPLING_RULES_FILE
// DD_SPAN_SAMPLING_RULES and DD_SPAN_SAMPLING_RULES_FILE environment variables.
func samplingRulesFromEnv() (trace, span []SamplingRule, err error) {
	var errs []string
	defer func() {
		if len(errs) != 0 {
			err = fmt.Errorf("\n\t%s", strings.Join(errs, "\n\t"))
		}
	}()

	rulesByType := func(spanType SamplingRuleType) (rules []SamplingRule, errs []string) {
		env := fmt.Sprintf("DD_%s_SAMPLING_RULES", strings.ToUpper(spanType.String()))
		rulesEnv := os.Getenv(fmt.Sprintf("DD_%s_SAMPLING_RULES", strings.ToUpper(spanType.String())))
		rules, err := unmarshalSamplingRules([]byte(rulesEnv), spanType)
		if err != nil {
			errs = append(errs, err.Error())
		}
		rulesFile := os.Getenv(env + "_FILE")
		if len(rules) != 0 {
			if rulesFile != "" {
				log.Warn("DIAGNOSTICS Error(s): %s is available and will take precedence over %s_FILE", env, env)
			}
			return rules, errs
		}
		if rulesFile == "" {
			return rules, errs
		}
		rulesFromEnvFile, err := os.ReadFile(rulesFile)
		if err != nil {
			errs = append(errs, fmt.Sprintf("Couldn't read file from %s_FILE: %v", env, err))
		}
		rules, err = unmarshalSamplingRules(rulesFromEnvFile, spanType)
		if err != nil {
			errs = append(errs, err.Error())
		}
		return rules, errs
	}

	trace, tErrs := rulesByType(SamplingRuleTrace)
	if len(tErrs) != 0 {
		errs = append(errs, tErrs...)
	}
	span, sErrs := rulesByType(SamplingRuleSpan)
	if len(sErrs) != 0 {
		errs = append(errs, sErrs...)
	}
	return trace, span, err
}

func (sr *SamplingRule) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	var v jsonRule
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	rules, err := validateRules([]jsonRule{v}, SamplingRuleUndefined)
	if err != nil {
		return err
	}
	*sr = rules[0]
	return nil
}

type jsonRule struct {
	Service      string            `json:"service"`
	Name         string            `json:"name"`
	Rate         json.Number       `json:"sample_rate"`
	MaxPerSecond float64           `json:"max_per_second"`
	Resource     string            `json:"resource"`
	Tags         map[string]string `json:"tags"`
	Type         *SamplingRuleType `json:"type,omitempty"`
}

func (j jsonRule) String() string {
	var s []string
	if j.Service != "" {
		s = append(s, fmt.Sprintf("Service:%s", j.Service))
	}
	if j.Name != "" {
		s = append(s, fmt.Sprintf("Name:%s", j.Name))
	}
	if j.Rate != "" {
		s = append(s, fmt.Sprintf("Rate:%s", j.Rate))
	}
	if j.MaxPerSecond != 0 {
		s = append(s, fmt.Sprintf("MaxPerSecond:%f", j.MaxPerSecond))
	}
	if j.Resource != "" {
		s = append(s, fmt.Sprintf("Resource:%s", j.Resource))
	}
	if len(j.Tags) != 0 {
		s = append(s, fmt.Sprintf("Tags:%v", j.Tags))
	}
	if j.Type != nil {
		s = append(s, fmt.Sprintf("Type: %v", *j.Type))
	}
	return fmt.Sprintf("{%s}", strings.Join(s, " "))
}

// unmarshalSamplingRules unmarshals JSON from b and returns the sampling rules found, attributing
// the type t to them. If any errors are occurred, they are returned.
func unmarshalSamplingRules(b []byte, spanType SamplingRuleType) ([]SamplingRule, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var jsonRules []jsonRule
	//	 if the JSON is an array, unmarshal it as an array of rules
	err := json.Unmarshal(b, &jsonRules)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling JSON: %v", err)
	}
	return validateRules(jsonRules, spanType)
}

func validateRules(jsonRules []jsonRule, spanType SamplingRuleType) ([]SamplingRule, error) {
	var errs []string
	rules := make([]SamplingRule, 0, len(jsonRules))
	for i, v := range jsonRules {
		if v.Rate == "" {
			v.Rate = "1"
		}
		if v.Type != nil && *v.Type != spanType {
			spanType = *v.Type
		}
		rate, err := v.Rate.Float64()
		if err != nil {
			errs = append(errs, fmt.Sprintf("at index %d: %v", i, err))
			continue
		}
		if rate < 0.0 || rate > 1.0 {
			errs = append(errs, fmt.Sprintf("at index %d: ignoring rule %s: rate is out of [0.0, 1.0] range", i, v.String()))
			continue
		}
		tagGlobs := make(map[string]*regexp.Regexp, len(v.Tags))
		for k, g := range v.Tags {
			tagGlobs[k] = globMatch(g)
		}
		rules = append(rules, SamplingRule{
			Service:      globMatch(v.Service),
			Name:         globMatch(v.Name),
			Rate:         rate,
			MaxPerSecond: v.MaxPerSecond,
			Resource:     globMatch(v.Resource),
			Tags:         tagGlobs,
			ruleType:     spanType,
			limiter:      newSingleSpanRateLimiter(v.MaxPerSecond),
		})
	}
	if len(errs) != 0 {
		return rules, fmt.Errorf("%s", strings.Join(errs, "\n\t"))
	}
	return rules, nil
}

// MarshalJSON implements the json.Marshaler interface.
func (sr *SamplingRule) MarshalJSON() ([]byte, error) {
	s := struct {
		Service      string            `json:"service,omitempty"`
		Name         string            `json:"name,omitempty"`
		Resource     string            `json:"resource,omitempty"`
		Rate         float64           `json:"sample_rate"`
		Tags         map[string]string `json:"tags,omitempty"`
		Type         *string           `json:"type,omitempty"`
		MaxPerSecond *float64          `json:"max_per_second,omitempty"`
	}{}
	if sr.Service != nil {
		s.Service = sr.Service.String()
	}
	if sr.Name != nil {
		s.Name = sr.Name.String()
	}
	if sr.MaxPerSecond != 0 {
		s.MaxPerSecond = &sr.MaxPerSecond
	}
	if sr.Resource != nil {
		s.Resource = sr.Resource.String()
	}
	s.Rate = sr.Rate
	if v := sr.ruleType.String(); v != "" {
		t := fmt.Sprintf("%v(%d)", v, sr.ruleType)
		s.Type = &t
	}
	s.Tags = make(map[string]string, len(sr.Tags))
	for k, v := range sr.Tags {
		if v != nil {
			s.Tags[k] = v.String()
		}
	}
	return json.Marshal(&s)
}
