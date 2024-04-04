// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/ext"
	"gopkg.in/DataDog/dd-trace-go.v1/internal"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/globalconfig"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/namingschema"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/normalizer"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/traceprof"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/version"

	"github.com/DataDog/datadog-go/v5/statsd"
)

var contribIntegrations = map[string]struct {
	name     string // user readable name for startup logs
	imported bool   // true if the user has imported the integration
}{
	"github.com/99designs/gqlgen":                   {"gqlgen", false},
	"github.com/aws/aws-sdk-go":                     {"AWS SDK", false},
	"github.com/aws/aws-sdk-go-v2":                  {"AWS SDK v2", false},
	"github.com/bradfitz/gomemcache":                {"Memcache", false},
	"cloud.google.com/go/pubsub.v1":                 {"Pub/Sub", false},
	"github.com/confluentinc/confluent-kafka-go":    {"Kafka (confluent)", false},
	"github.com/confluentinc/confluent-kafka-go/v2": {"Kafka (confluent) v2", false},
	"database/sql":                                  {"SQL", false},
	"github.com/dimfeld/httptreemux/v5":             {"HTTP Treemux", false},
	"github.com/elastic/go-elasticsearch/v6":        {"Elasticsearch v6", false},
	"github.com/emicklei/go-restful":                {"go-restful", false},
	"github.com/emicklei/go-restful/v3":             {"go-restful v3", false},
	"github.com/garyburd/redigo":                    {"Redigo (dep)", false},
	"github.com/gin-gonic/gin":                      {"Gin", false},
	"github.com/globalsign/mgo":                     {"MongoDB (mgo)", false},
	"github.com/go-chi/chi":                         {"chi", false},
	"github.com/go-chi/chi/v5":                      {"chi v5", false},
	"github.com/go-pg/pg/v10":                       {"go-pg v10", false},
	"github.com/go-redis/redis":                     {"Redis", false},
	"github.com/go-redis/redis/v7":                  {"Redis v7", false},
	"github.com/go-redis/redis/v8":                  {"Redis v8", false},
	"go.mongodb.org/mongo-driver":                   {"MongoDB", false},
	"github.com/gocql/gocql":                        {"Cassandra", false},
	"github.com/gofiber/fiber/v2":                   {"Fiber", false},
	"github.com/gomodule/redigo":                    {"Redigo", false},
	"google.golang.org/api":                         {"Google API", false},
	"google.golang.org/grpc":                        {"gRPC", false},
	"google.golang.org/grpc/v12":                    {"gRPC v12", false},
	"gopkg.in/jinzhu/gorm.v1":                       {"Gorm (gopkg)", false},
	"github.com/gorilla/mux":                        {"Gorilla Mux", false},
	"gorm.io/gorm.v1":                               {"Gorm v1", false},
	"github.com/graph-gophers/graphql-go":           {"GraphQL", false},
	"github.com/hashicorp/consul/api":               {"Consul", false},
	"github.com/hashicorp/vault/api":                {"Vault", false},
	"github.com/jinzhu/gorm":                        {"Gorm", false},
	"github.com/jmoiron/sqlx":                       {"SQLx", false},
	"github.com/julienschmidt/httprouter":           {"HTTP Router", false},
	"k8s.io/client-go/kubernetes":                   {"Kubernetes", false},
	"github.com/labstack/echo":                      {"echo", false},
	"github.com/labstack/echo/v4":                   {"echo v4", false},
	"github.com/miekg/dns":                          {"miekg/dns", false},
	"net/http":                                      {"HTTP", false},
	"gopkg.in/olivere/elastic.v5":                   {"Elasticsearch v5", false},
	"gopkg.in/olivere/elastic.v3":                   {"Elasticsearch v3", false},
	"github.com/redis/go-redis/v9":                  {"Redis v9", false},
	"github.com/segmentio/kafka-go":                 {"Kafka v0", false},
	"github.com/IBM/sarama":                         {"IBM sarama", false},
	"github.com/Shopify/sarama":                     {"Shopify sarama", false},
	"github.com/sirupsen/logrus":                    {"Logrus", false},
	"github.com/syndtr/goleveldb":                   {"LevelDB", false},
	"github.com/tidwall/buntdb":                     {"BuntDB", false},
	"github.com/twitchtv/twirp":                     {"Twirp", false},
	"github.com/urfave/negroni":                     {"Negroni", false},
	"github.com/valyala/fasthttp":                   {"FastHTTP", false},
	"github.com/zenazn/goji":                        {"Goji", false},
}

var (
	// defaultSocketAPM specifies the socket path to use for connecting to the trace-agent.
	// Replaced in tests
	defaultSocketAPM = "/var/run/datadog/apm.socket"

	// defaultSocketDSD specifies the socket path to use for connecting to the statsd server.
	// Replaced in tests
	defaultSocketDSD = "/var/run/datadog/dsd.socket"

	// defaultMaxTagsHeaderLen specifies the default maximum length of the X-Datadog-Tags header value.
	defaultMaxTagsHeaderLen = 128
)

// config holds the tracer configuration.
type config struct {
	// debug, when true, writes details to logs.
	debug bool

	// agent holds the capabilities of the agent and determines some
	// of the behaviour of the tracer.
	agent agentFeatures

	// integrations reports if the user has instrumented a Datadog integration and
	// if they have a version of the library available to integrate.
	integrations map[string]integrationConfig

	// featureFlags specifies any enabled feature flags.
	featureFlags map[string]struct{}

	// logToStdout reports whether we should log all traces to the standard
	// output instead of using the agent. This is used in Lambda environments.
	logToStdout bool

	// sendRetries is the number of times a trace payload send is retried upon
	// failure.
	sendRetries int

	// logStartup, when true, causes various startup info to be written
	// when the tracer starts.
	logStartup bool

	// serviceName specifies the name of this application.
	serviceName string

	// universalVersion, reports whether span service name and config service name
	// should match to set application version tag. False by default
	universalVersion bool

	// version specifies the version of this application
	version string

	// env contains the environment that this application will run under.
	env string

	// sampler specifies the sampler that will be used for sampling traces.
	sampler Sampler

	// agentURL is the agent URL that receives traces from the tracer.
	agentURL *url.URL

	// serviceMappings holds a set of service mappings to dynamically rename services
	serviceMappings map[string]string

	// globalTags holds a set of tags that will be automatically applied to
	// all spans.
	globalTags dynamicConfig[map[string]interface{}]

	// transport specifies the Transport interface which will be used to send data to the agent.
	transport transport

	// propagator propagates span context cross-process
	propagator Propagator

	// httpClient specifies the HTTP client to be used by the agent's transport.
	httpClient *http.Client

	// hostname is automatically assigned when the DD_TRACE_REPORT_HOSTNAME is set to true,
	// and is added as a special tag to the root span of traces.
	hostname string

	// logger specifies the logger to use when printing errors. If not specified, the "log" package
	// will be used.
	logger ddtrace.Logger

	// runtimeMetrics specifies whether collection of runtime metrics is enabled.
	runtimeMetrics bool

	// dogstatsdAddr specifies the address to connect for sending metrics to the
	// Datadog Agent. If not set, it defaults to "localhost:8125" or to the
	// combination of the environment variables DD_AGENT_HOST and DD_DOGSTATSD_PORT.
	dogstatsdAddr string

	// statsdClient is set when a user provides a custom statsd client for tracking metrics
	// associated with the runtime and the tracer.
	statsdClient internal.StatsdClient

	// spanRules contains user-defined rules to determine the sampling rate to apply
	// to a single span without affecting the entire trace
	spanRules []SamplingRule

	// traceRules contains user-defined rules to determine the sampling rate to apply
	// to the entire trace if any spans satisfy the criteria
	traceRules []SamplingRule

	// tickChan specifies a channel which will receive the time every time the tracer must flush.
	// It defaults to time.Ticker; replaced in tests.
	tickChan <-chan time.Time

	// noDebugStack disables the collection of debug stack traces globally. No traces reporting
	// errors will record a stack trace when this option is set.
	noDebugStack bool

	// profilerHotspots specifies whether profiler Code Hotspots is enabled.
	profilerHotspots bool

	// profilerEndpoints specifies whether profiler endpoint filtering is enabled.
	profilerEndpoints bool

	// enabled reports whether tracing is enabled.
	enabled dynamicConfig[bool]

	// enableHostnameDetection specifies whether the tracer should enable hostname detection.
	enableHostnameDetection bool

	// spanAttributeSchemaVersion holds the selected DD_TRACE_SPAN_ATTRIBUTE_SCHEMA version.
	spanAttributeSchemaVersion int

	// peerServiceDefaultsEnabled indicates whether the peer.service tag calculation is enabled or not.
	peerServiceDefaultsEnabled bool

	// peerServiceMappings holds a set of service mappings to dynamically rename peer.service values.
	peerServiceMappings map[string]string

	// debugAbandonedSpans controls if the tracer should log when old, open spans are found
	debugAbandonedSpans bool

	// spanTimeout represents how old a span can be before it should be logged as a possible
	// misconfiguration
	spanTimeout time.Duration

	// partialFlushMinSpans is the number of finished spans in a single trace to trigger a
	// partial flush, or 0 if partial flushing is disabled.
	// Value from DD_TRACE_PARTIAL_FLUSH_MIN_SPANS, default 1000.
	partialFlushMinSpans int

	// partialFlushEnabled specifices whether the tracer should enable partial flushing. Value
	// from DD_TRACE_PARTIAL_FLUSH_ENABLED, default false.
	partialFlushEnabled bool

	// statsComputationEnabled enables client-side stats computation (aka trace metrics).
	statsComputationEnabled bool

	// dataStreamsMonitoringEnabled specifies whether the tracer should enable monitoring of data streams
	dataStreamsMonitoringEnabled bool

	// orchestrionCfg holds Orchestrion (aka auto-instrumentation) configuration.
	// Only used for telemetry currently.
	orchestrionCfg orchestrionConfig

	// traceSampleRate holds the trace sample rate.
	traceSampleRate dynamicConfig[float64]

	// headerAsTags holds the header as tags configuration.
	headerAsTags dynamicConfig[[]string]

	contribStats bool

	// dynamicInstrumentationEnabled controls if the target application can be modified by Dynamic Instrumentation or not.
	// Value from DD_DYNAMIC_INSTRUMENTATION_ENABLED, default false.
	dynamicInstrumentationEnabled bool
}

// orchestrionConfig contains Orchestrion configuration.
type orchestrionConfig struct {
	// Enabled indicates whether this tracer was instanciated via Orchestrion.
	Enabled bool `json:"enabled"`

	// Metadata holds Orchestrion specific metadata (e.g orchestrion version, mode (toolexec or manual) etc..)
	Metadata map[string]string `json:"metadata,omitempty"`
}

// HasFeature reports whether feature f is enabled.
func (c *config) HasFeature(f string) bool {
	_, ok := c.featureFlags[strings.TrimSpace(f)]
	return ok
}

// StartOption represents a function that can be provided as a parameter to Start.
type StartOption func(*config)

// maxPropagatedTagsLength limits the size of DD_TRACE_X_DATADOG_TAGS_MAX_LENGTH to prevent HTTP 413 responses.
const maxPropagatedTagsLength = 512

// partialFlushMinSpansDefault is the default number of spans for partial flushing, if enabled.
const partialFlushMinSpansDefault = 1000

// newConfig renders the tracer configuration based on defaults, environment variables
// and passed user opts.
func newConfig(opts ...StartOption) *config {
	c := new(config)
	c.sampler = NewAllSampler()

	if internal.BoolEnv("DD_TRACE_ANALYTICS_ENABLED", false) {
		globalconfig.SetAnalyticsRate(1.0)
	}
	if os.Getenv("DD_TRACE_REPORT_HOSTNAME") == "true" {
		var err error
		c.hostname, err = os.Hostname()
		if err != nil {
			log.Warn("unable to look up hostname: %v", err)
		}
	}
	if v := os.Getenv("DD_TRACE_SOURCE_HOSTNAME"); v != "" {
		c.hostname = v
	}
	if v := os.Getenv("DD_ENV"); v != "" {
		c.env = v
	}
	if v := os.Getenv("DD_TRACE_FEATURES"); v != "" {
		WithFeatureFlags(strings.FieldsFunc(v, func(r rune) bool {
			return r == ',' || r == ' '
		})...)(c)
	}
	if v := os.Getenv("DD_SERVICE"); v != "" {
		c.serviceName = v
		globalconfig.SetServiceName(v)
	}
	if ver := os.Getenv("DD_VERSION"); ver != "" {
		c.version = ver
	}
	if v := os.Getenv("DD_SERVICE_MAPPING"); v != "" {
		internal.ForEachStringTag(v, func(key, val string) { WithServiceMapping(key, val)(c) })
	}
	c.headerAsTags = newDynamicConfig("trace_header_tags", nil, setHeaderTags, equalSlice[string])
	if v := os.Getenv("DD_TRACE_HEADER_TAGS"); v != "" {
		WithHeaderTags(strings.Split(v, ","))(c)
	}
	if v := os.Getenv("DD_TAGS"); v != "" {
		tags := internal.ParseTagString(v)
		internal.CleanGitMetadataTags(tags)
		for key, val := range tags {
			WithGlobalTag(key, val)(c)
		}
	}
	if _, ok := os.LookupEnv("AWS_LAMBDA_FUNCTION_NAME"); ok {
		// AWS_LAMBDA_FUNCTION_NAME being set indicates that we're running in an AWS Lambda environment.
		// See: https://docs.aws.amazon.com/lambda/latest/dg/configuration-envvars.html
		c.logToStdout = true
	}
	c.logStartup = internal.BoolEnv("DD_TRACE_STARTUP_LOGS", true)
	c.contribStats = internal.BoolEnv("DD_TRACE_CONTRIB_STATS_ENABLED", true)
	c.runtimeMetrics = internal.BoolEnv("DD_RUNTIME_METRICS_ENABLED", false)
	c.debug = internal.BoolEnv("DD_TRACE_DEBUG", false)
	c.enabled = newDynamicConfig("tracing_enabled", internal.BoolEnv("DD_TRACE_ENABLED", true), func(b bool) bool { return true }, equal[bool])
	c.profilerEndpoints = internal.BoolEnv(traceprof.EndpointEnvVar, true)
	c.profilerHotspots = internal.BoolEnv(traceprof.CodeHotspotsEnvVar, true)
	c.enableHostnameDetection = internal.BoolEnv("DD_CLIENT_HOSTNAME_ENABLED", true)
	c.debugAbandonedSpans = internal.BoolEnv("DD_TRACE_DEBUG_ABANDONED_SPANS", false)
	if c.debugAbandonedSpans {
		c.spanTimeout = internal.DurationEnv("DD_TRACE_ABANDONED_SPAN_TIMEOUT", 10*time.Minute)
	}
	c.statsComputationEnabled = internal.BoolEnv("DD_TRACE_STATS_COMPUTATION_ENABLED", false)
	c.dataStreamsMonitoringEnabled = internal.BoolEnv("DD_DATA_STREAMS_ENABLED", false)
	c.partialFlushEnabled = internal.BoolEnv("DD_TRACE_PARTIAL_FLUSH_ENABLED", false)
	c.partialFlushMinSpans = internal.IntEnv("DD_TRACE_PARTIAL_FLUSH_MIN_SPANS", partialFlushMinSpansDefault)
	if c.partialFlushMinSpans <= 0 {
		log.Warn("DD_TRACE_PARTIAL_FLUSH_MIN_SPANS=%d is not a valid value, setting to default %d", c.partialFlushMinSpans, partialFlushMinSpansDefault)
		c.partialFlushMinSpans = partialFlushMinSpansDefault
	} else if c.partialFlushMinSpans >= traceMaxSize {
		log.Warn("DD_TRACE_PARTIAL_FLUSH_MIN_SPANS=%d is above the max number of spans that can be kept in memory for a single trace (%d spans), so partial flushing will never trigger, setting to default %d", c.partialFlushMinSpans, traceMaxSize, partialFlushMinSpansDefault)
		c.partialFlushMinSpans = partialFlushMinSpansDefault
	}
	// TODO(partialFlush): consider logging a warning if DD_TRACE_PARTIAL_FLUSH_MIN_SPANS
	// is set, but DD_TRACE_PARTIAL_FLUSH_ENABLED is not true. Or just assume it should be enabled
	// if it's explicitly set, and don't require both variables to be configured.

	c.dynamicInstrumentationEnabled = internal.BoolEnv("DD_DYNAMIC_INSTRUMENTATION_ENABLED", false)

	schemaVersionStr := os.Getenv("DD_TRACE_SPAN_ATTRIBUTE_SCHEMA")
	if v, ok := namingschema.ParseVersion(schemaVersionStr); ok {
		namingschema.SetVersion(v)
		c.spanAttributeSchemaVersion = int(v)
	} else {
		v := namingschema.SetDefaultVersion()
		c.spanAttributeSchemaVersion = int(v)
		log.Warn("DD_TRACE_SPAN_ATTRIBUTE_SCHEMA=%s is not a valid value, setting to default of v%d", schemaVersionStr, v)
	}
	// Allow DD_TRACE_SPAN_ATTRIBUTE_SCHEMA=v0 users to disable default integration (contrib AKA v0) service names.
	// These default service names are always disabled for v1 onwards.
	namingschema.SetUseGlobalServiceName(internal.BoolEnv("DD_TRACE_REMOVE_INTEGRATION_SERVICE_NAMES_ENABLED", false))

	// peer.service tag default calculation is enabled by default if using attribute schema >= 1
	c.peerServiceDefaultsEnabled = true
	if c.spanAttributeSchemaVersion == int(namingschema.SchemaV0) {
		c.peerServiceDefaultsEnabled = internal.BoolEnv("DD_TRACE_PEER_SERVICE_DEFAULTS_ENABLED", false)
	}
	c.peerServiceMappings = make(map[string]string)
	if v := os.Getenv("DD_TRACE_PEER_SERVICE_MAPPING"); v != "" {
		internal.ForEachStringTag(v, func(key, val string) { c.peerServiceMappings[key] = val })
	}

	for _, fn := range opts {
		fn(c)
	}
	if c.agentURL == nil {
		c.agentURL = resolveAgentAddr()
		if url := internal.AgentURLFromEnv(); url != nil {
			c.agentURL = url
		}
	}
	if c.agentURL.Scheme == "unix" {
		// If we're connecting over UDS we can just rely on the agent to provide the hostname
		log.Debug("connecting to agent over unix, do not set hostname on any traces")
		c.enableHostnameDetection = false
		c.httpClient = udsClient(c.agentURL.Path)
		c.agentURL = &url.URL{
			Scheme: "http",
			Host:   fmt.Sprintf("UDS_%s", strings.NewReplacer(":", "_", "/", "_", `\`, "_").Replace(c.agentURL.Path)),
		}
	} else if c.httpClient == nil {
		c.httpClient = defaultClient
	}
	WithGlobalTag(ext.RuntimeID, globalconfig.RuntimeID())(c)
	globalTags := c.globalTags.get()
	if c.env == "" {
		if v, ok := globalTags["env"]; ok {
			if e, ok := v.(string); ok {
				c.env = e
			}
		}
	}
	if c.version == "" {
		if v, ok := globalTags["version"]; ok {
			if ver, ok := v.(string); ok {
				c.version = ver
			}
		}
	}
	if c.serviceName == "" {
		if v, ok := globalTags["service"]; ok {
			if s, ok := v.(string); ok {
				c.serviceName = s
				globalconfig.SetServiceName(s)
			}
		} else {
			// There is not an explicit service set, default to binary name.
			// In this case, don't set a global service name so the contribs continue using their defaults.
			c.serviceName = filepath.Base(os.Args[0])
		}
	}
	if c.transport == nil {
		c.transport = newHTTPTransport(c.agentURL.String(), c.httpClient)
	}
	if c.propagator == nil {
		envKey := "DD_TRACE_X_DATADOG_TAGS_MAX_LENGTH"
		max := internal.IntEnv(envKey, defaultMaxTagsHeaderLen)
		if max < 0 {
			log.Warn("Invalid value %d for %s. Setting to 0.", max, envKey)
			max = 0
		}
		if max > maxPropagatedTagsLength {
			log.Warn("Invalid value %d for %s. Maximum allowed is %d. Setting to %d.", max, envKey, maxPropagatedTagsLength, maxPropagatedTagsLength)
			max = maxPropagatedTagsLength
		}
		c.propagator = NewPropagator(&PropagatorConfig{
			MaxTagsHeaderLen: max,
		})
	}
	if c.logger != nil {
		log.UseLogger(c.logger)
	}
	if c.debug {
		log.SetLevel(log.LevelDebug)
	}
	c.agent = loadAgentFeatures(c.logToStdout, c.agentURL, c.httpClient)
	info, ok := debug.ReadBuildInfo()
	if !ok {
		c.loadContribIntegrations([]*debug.Module{})
	} else {
		c.loadContribIntegrations(info.Deps)
	}
	if c.statsdClient == nil {
		// configure statsd client
		addr := c.dogstatsdAddr
		if addr == "" {
			// no config defined address; use defaults
			addr = defaultDogstatsdAddr()
		}
		if agentport := c.agent.StatsdPort; agentport > 0 {
			// the agent reported a non-standard port
			host, _, err := net.SplitHostPort(addr)
			if err == nil {
				// we have a valid host:port address; replace the port because
				// the agent knows better
				if host == "" {
					host = defaultHostname
				}
				addr = net.JoinHostPort(host, strconv.Itoa(agentport))
			}
			// not a valid TCP address, leave it as it is (could be a socket connection)
		}
		c.dogstatsdAddr = addr
	}
	// Re-initialize the globalTags config with the value constructed from the environment and start options
	// This allows persisting the initial value of globalTags for future resets and updates.
	c.initGlobalTags(c.globalTags.get())

	return c
}

func newStatsdClient(c *config) (internal.StatsdClient, error) {
	if c.statsdClient != nil {
		return c.statsdClient, nil
	}

	client, err := statsd.New(c.dogstatsdAddr, statsd.WithMaxMessagesPerPayload(40), statsd.WithTags(statsTags(c)))
	if err != nil {
		return &statsd.NoOpClient{}, err
	}
	return client, nil
}

// defaultHTTPClient returns the default http.Client to start the tracer with.
func defaultHTTPClient() *http.Client {
	if _, err := os.Stat(defaultSocketAPM); err == nil {
		// we have the UDS socket file, use it
		return udsClient(defaultSocketAPM)
	}
	return defaultClient
}

// udsClient returns a new http.Client which connects using the given UDS socket path.
func udsClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return defaultDialer.DialContext(ctx, "unix", (&net.UnixAddr{
					Name: socketPath,
					Net:  "unix",
				}).String())
			},
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		Timeout: defaultHTTPTimeout,
	}
}

// defaultDogstatsdAddr returns the default connection address for Dogstatsd.
func defaultDogstatsdAddr() string {
	envHost, envPort := os.Getenv("DD_AGENT_HOST"), os.Getenv("DD_DOGSTATSD_PORT")
	if _, err := os.Stat(defaultSocketDSD); err == nil && envHost == "" && envPort == "" {
		// socket exists and user didn't specify otherwise via env vars
		return "unix://" + defaultSocketDSD
	}
	host, port := defaultHostname, "8125"
	if envHost != "" {
		host = envHost
	}
	if envPort != "" {
		port = envPort
	}
	return net.JoinHostPort(host, port)
}

type integrationConfig struct {
	Instrumented bool   `json:"instrumented"`      // indicates if the user has imported and used the integration
	Available    bool   `json:"available"`         // indicates if the user is using a library that can be used with DataDog integrations
	Version      string `json:"available_version"` // if available, indicates the version of the library the user has
}

// agentFeatures holds information about the trace-agent's capabilities.
// When running WithLambdaMode, a zero-value of this struct will be used
// as features.
type agentFeatures struct {
	// DropP0s reports whether it's ok for the tracer to not send any
	// P0 traces to the agent.
	DropP0s bool

	// Stats reports whether the agent can receive client-computed stats on
	// the /v0.6/stats endpoint.
	Stats bool

	// DataStreams reports whether the agent can receive data streams stats on
	// the /v0.1/pipeline_stats endpoint.
	DataStreams bool

	// StatsdPort specifies the Dogstatsd port as provided by the agent.
	// If it's the default, it will be 0, which means 8125.
	StatsdPort int

	// featureFlags specifies all the feature flags reported by the trace-agent.
	featureFlags map[string]struct{}
}

// HasFlag reports whether the agent has set the feat feature flag.
func (a *agentFeatures) HasFlag(feat string) bool {
	_, ok := a.featureFlags[feat]
	return ok
}

// loadAgentFeatures queries the trace-agent for its capabilities and updates
// the tracer's behaviour.
func loadAgentFeatures(logToStdout bool, agentURL *url.URL, httpClient *http.Client) (features agentFeatures) {
	if logToStdout {
		// there is no agent; all features off
		return
	}
	resp, err := httpClient.Get(fmt.Sprintf("%s/info", agentURL))
	if err != nil {
		log.Error("Loading features: %v", err)
		return
	}
	if resp.StatusCode == http.StatusNotFound {
		// agent is older than 7.28.0, features not discoverable
		return
	}
	defer resp.Body.Close()
	type infoResponse struct {
		Endpoints     []string `json:"endpoints"`
		ClientDropP0s bool     `json:"client_drop_p0s"`
		StatsdPort    int      `json:"statsd_port"`
		FeatureFlags  []string `json:"feature_flags"`
	}
	var info infoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		log.Error("Decoding features: %v", err)
		return
	}
	features.DropP0s = info.ClientDropP0s
	features.StatsdPort = info.StatsdPort
	for _, endpoint := range info.Endpoints {
		switch endpoint {
		case "/v0.6/stats":
			features.Stats = true
		case "/v0.1/pipeline_stats":
			features.DataStreams = true
		}
	}
	features.featureFlags = make(map[string]struct{}, len(info.FeatureFlags))
	for _, flag := range info.FeatureFlags {
		features.featureFlags[flag] = struct{}{}
	}
	return features
}

// MarkIntegrationImported labels the given integration as imported
func MarkIntegrationImported(integration string) bool {
	s, ok := contribIntegrations[integration]
	if !ok {
		return false
	}
	s.imported = true
	contribIntegrations[integration] = s
	return true
}

func (c *config) loadContribIntegrations(deps []*debug.Module) {
	integrations := map[string]integrationConfig{}
	for _, s := range contribIntegrations {
		integrations[s.name] = integrationConfig{
			Instrumented: s.imported,
		}
	}
	for _, d := range deps {
		p := d.Path
		// special use case, since gRPC does not update version number
		if p == "google.golang.org/grpc" {
			re := regexp.MustCompile(`v(\d.\d)\d*`)
			match := re.FindStringSubmatch(d.Version)
			if match == nil {
				log.Warn("Unable to parse version of GRPC %v", d.Version)
				continue
			}
			ver, err := strconv.ParseFloat(match[1], 32)
			if err != nil {
				log.Warn("Unable to parse version of GRPC %v as a float", d.Version)
				continue
			}
			if ver <= 1.2 {
				p = p + "/v12"
			}
		}
		s, ok := contribIntegrations[p]
		if !ok {
			continue
		}
		conf := integrations[s.name]
		conf.Available = true
		conf.Version = d.Version
		integrations[s.name] = conf
	}
	c.integrations = integrations
}

func (c *config) canComputeStats() bool {
	return c.agent.Stats && (c.HasFeature("discovery") || c.statsComputationEnabled)
}

func (c *config) canDropP0s() bool {
	return c.canComputeStats() && c.agent.DropP0s
}

func statsTags(c *config) []string {
	tags := []string{
		"lang:go",
		"version:" + version.Tag,
		"lang_version:" + runtime.Version(),
	}
	if c.serviceName != "" {
		tags = append(tags, "service:"+c.serviceName)
	}
	if c.env != "" {
		tags = append(tags, "env:"+c.env)
	}
	if c.hostname != "" {
		tags = append(tags, "host:"+c.hostname)
	}
	for k, v := range c.globalTags.get() {
		if vstr, ok := v.(string); ok {
			tags = append(tags, k+":"+vstr)
		}
	}
	return tags
}

// withNoopStats is used for testing to disable statsd client
func withNoopStats() StartOption {
	return func(c *config) {
		c.statsdClient = &statsd.NoOpClient{}
	}
}

// WithFeatureFlags specifies a set of feature flags to enable. Please take into account
// that most, if not all features flags are considered to be experimental and result in
// unexpected bugs.
func WithFeatureFlags(feats ...string) StartOption {
	return func(c *config) {
		if c.featureFlags == nil {
			c.featureFlags = make(map[string]struct{}, len(feats))
		}
		for _, f := range feats {
			c.featureFlags[strings.TrimSpace(f)] = struct{}{}
		}
		log.Info("FEATURES enabled: %v", feats)
	}
}

// WithLogger sets logger as the tracer's error printer.
// Diagnostic and startup tracer logs are prefixed to simplify the search within logs.
// If JSON logging format is required, it's possible to wrap tracer logs using an existing JSON logger with this
// function. To learn more about this possibility, please visit: https://github.com/DataDog/dd-trace-go/issues/2152#issuecomment-1790586933
func WithLogger(logger ddtrace.Logger) StartOption {
	return func(c *config) {
		c.logger = logger
	}
}

// WithPrioritySampling is deprecated, and priority sampling is enabled by default.
// When using distributed tracing, the priority sampling value is propagated in order to
// get all the parts of a distributed trace sampled.
// To learn more about priority sampling, please visit:
// https://docs.datadoghq.com/tracing/getting_further/trace_sampling_and_storage/#priority-sampling-for-distributed-tracing
func WithPrioritySampling() StartOption {
	return func(c *config) {
		// This is now enabled by default.
	}
}

// WithDebugStack can be used to globally enable or disable the collection of stack traces when
// spans finish with errors. It is enabled by default. This is a global version of the NoDebugStack
// FinishOption.
func WithDebugStack(enabled bool) StartOption {
	return func(c *config) {
		c.noDebugStack = !enabled
	}
}

// WithDebugMode enables debug mode on the tracer, resulting in more verbose logging.
func WithDebugMode(enabled bool) StartOption {
	return func(c *config) {
		c.debug = enabled
	}
}

// WithLambdaMode enables lambda mode on the tracer, for use with AWS Lambda.
// This option is only required if the the Datadog Lambda Extension is not
// running.
func WithLambdaMode(enabled bool) StartOption {
	return func(c *config) {
		c.logToStdout = enabled
	}
}

// WithSendRetries enables re-sending payloads that are not successfully
// submitted to the agent.  This will cause the tracer to retry the send at
// most `retries` times.
func WithSendRetries(retries int) StartOption {
	return func(c *config) {
		c.sendRetries = retries
	}
}

// WithPropagator sets an alternative propagator to be used by the tracer.
func WithPropagator(p Propagator) StartOption {
	return func(c *config) {
		c.propagator = p
	}
}

// WithServiceName is deprecated. Please use WithService.
// If you are using an older version and you are upgrading from WithServiceName
// to WithService, please note that WithService will determine the service name of
// server and framework integrations.
func WithServiceName(name string) StartOption {
	return func(c *config) {
		c.serviceName = name
		if globalconfig.ServiceName() != "" {
			log.Warn("ddtrace/tracer: deprecated config WithServiceName should not be used " +
				"with `WithService` or `DD_SERVICE`; integration service name will not be set.")
		}
		globalconfig.SetServiceName("")
	}
}

// WithService sets the default service name for the program.
func WithService(name string) StartOption {
	return func(c *config) {
		c.serviceName = name
		globalconfig.SetServiceName(c.serviceName)
	}
}

// WithGlobalServiceName causes contrib libraries to use the global service name and not any locally defined service name.
// This is synonymous with `DD_TRACE_REMOVE_INTEGRATION_SERVICE_NAMES_ENABLED`.
func WithGlobalServiceName(enabled bool) StartOption {
	return func(_ *config) {
		namingschema.SetUseGlobalServiceName(enabled)
	}
}

// WithAgentAddr sets the address where the agent is located. The default is
// localhost:8126. It should contain both host and port.
func WithAgentAddr(addr string) StartOption {
	return func(c *config) {
		c.agentURL = &url.URL{
			Scheme: "http",
			Host:   addr,
		}
	}
}

// WithEnv sets the environment to which all traces started by the tracer will be submitted.
// The default value is the environment variable DD_ENV, if it is set.
func WithEnv(env string) StartOption {
	return func(c *config) {
		c.env = env
	}
}

// WithServiceMapping determines service "from" to be renamed to service "to".
// This option is is case sensitive and can be used multiple times.
func WithServiceMapping(from, to string) StartOption {
	return func(c *config) {
		if c.serviceMappings == nil {
			c.serviceMappings = make(map[string]string)
		}
		c.serviceMappings[from] = to
	}
}

// WithPeerServiceDefaults sets default calculation for peer.service.
func WithPeerServiceDefaults(enabled bool) StartOption {
	// TODO: add link to public docs
	return func(c *config) {
		c.peerServiceDefaultsEnabled = enabled
	}
}

// WithPeerServiceMapping determines the value of the peer.service tag "from" to be renamed to service "to".
func WithPeerServiceMapping(from, to string) StartOption {
	return func(c *config) {
		if c.peerServiceMappings == nil {
			c.peerServiceMappings = make(map[string]string)
		}
		c.peerServiceMappings[from] = to
	}
}

// WithGlobalTag sets a key/value pair which will be set as a tag on all spans
// created by tracer. This option may be used multiple times.
func WithGlobalTag(k string, v interface{}) StartOption {
	return func(c *config) {
		if c.globalTags.get() == nil {
			c.initGlobalTags(map[string]interface{}{})
		}
		c.globalTags.Lock()
		defer c.globalTags.Unlock()
		c.globalTags.current[k] = v
	}
}

// initGlobalTags initializes the globalTags config with the provided init value
func (c *config) initGlobalTags(init map[string]interface{}) {
	apply := func(map[string]interface{}) bool {
		// always set the runtime ID on updates
		c.globalTags.current[ext.RuntimeID] = globalconfig.RuntimeID()
		return true
	}
	c.globalTags = newDynamicConfig[map[string]interface{}]("trace_tags", init, apply, equalMap[string])
}

// WithSampler sets the given sampler to be used with the tracer. By default
// an all-permissive sampler is used.
func WithSampler(s Sampler) StartOption {
	return func(c *config) {
		c.sampler = s
	}
}

// WithHTTPRoundTripper is deprecated. Please consider using WithHTTPClient instead.
// The function allows customizing the underlying HTTP transport for emitting spans.
func WithHTTPRoundTripper(r http.RoundTripper) StartOption {
	return WithHTTPClient(&http.Client{
		Transport: r,
		Timeout:   defaultHTTPTimeout,
	})
}

// WithHTTPClient specifies the HTTP client to use when emitting spans to the agent.
func WithHTTPClient(client *http.Client) StartOption {
	return func(c *config) {
		c.httpClient = client
	}
}

// WithUDS configures the HTTP client to dial the Datadog Agent via the specified Unix Domain Socket path.
func WithUDS(socketPath string) StartOption {
	return func(c *config) {
		c.agentURL = &url.URL{
			Scheme: "unix",
			Path:   socketPath,
		}
	}
}

// WithAnalytics allows specifying whether Trace Search & Analytics should be enabled
// for integrations.
func WithAnalytics(on bool) StartOption {
	return func(cfg *config) {
		if on {
			globalconfig.SetAnalyticsRate(1.0)
		} else {
			globalconfig.SetAnalyticsRate(math.NaN())
		}
	}
}

// WithAnalyticsRate sets the global sampling rate for sampling APM events.
func WithAnalyticsRate(rate float64) StartOption {
	return func(_ *config) {
		if rate >= 0.0 && rate <= 1.0 {
			globalconfig.SetAnalyticsRate(rate)
		} else {
			globalconfig.SetAnalyticsRate(math.NaN())
		}
	}
}

// WithRuntimeMetrics enables automatic collection of runtime metrics every 10 seconds.
func WithRuntimeMetrics() StartOption {
	return func(cfg *config) {
		cfg.runtimeMetrics = true
	}
}

// WithDogstatsdAddress specifies the address to connect to for sending metrics to the Datadog
// Agent. It should be a "host:port" string, or the path to a unix domain socket.If not set, it
// attempts to determine the address of the statsd service according to the following rules:
//  1. Look for /var/run/datadog/dsd.socket and use it if present. IF NOT, continue to #2.
//  2. The host is determined by DD_AGENT_HOST, and defaults to "localhost"
//  3. The port is retrieved from the agent. If not present, it is determined by DD_DOGSTATSD_PORT, and defaults to 8125
//
// This option is in effect when WithRuntimeMetrics is enabled.
func WithDogstatsdAddress(addr string) StartOption {
	return func(cfg *config) {
		cfg.dogstatsdAddr = addr
	}
}

// WithSamplingRules specifies the sampling rates to apply to spans based on the
// provided rules.
func WithSamplingRules(rules []SamplingRule) StartOption {
	return func(cfg *config) {
		for _, rule := range rules {
			if rule.ruleType == SamplingRuleSpan {
				cfg.spanRules = append(cfg.spanRules, rule)
			} else {
				cfg.traceRules = append(cfg.traceRules, rule)
			}
		}
	}
}

// WithServiceVersion specifies the version of the service that is running. This will
// be included in spans from this service in the "version" tag, provided that
// span service name and config service name match. Do NOT use with WithUniversalVersion.
func WithServiceVersion(version string) StartOption {
	return func(cfg *config) {
		cfg.version = version
		cfg.universalVersion = false
	}
}

// WithUniversalVersion specifies the version of the service that is running, and will be applied to all spans,
// regardless of whether span service name and config service name match.
// See: WithService, WithServiceVersion. Do NOT use with WithServiceVersion.
func WithUniversalVersion(version string) StartOption {
	return func(c *config) {
		c.version = version
		c.universalVersion = true
	}
}

// WithHostname allows specifying the hostname with which to mark outgoing traces.
func WithHostname(name string) StartOption {
	return func(c *config) {
		c.hostname = name
	}
}

// WithTraceEnabled allows specifying whether tracing will be enabled
func WithTraceEnabled(enabled bool) StartOption {
	return func(c *config) {
		c.enabled = newDynamicConfig("tracing_enabled", enabled, func(b bool) bool { return true }, equal[bool])
	}
}

// WithLogStartup allows enabling or disabling the startup log.
func WithLogStartup(enabled bool) StartOption {
	return func(c *config) {
		c.logStartup = enabled
	}
}

// WithProfilerCodeHotspots enables the code hotspots integration between the
// tracer and profiler. This is done by automatically attaching pprof labels
// called "span id" and "local root span id" when new spans are created. You
// should not use these label names in your own code when this is enabled. The
// enabled value defaults to the value of the
// DD_PROFILING_CODE_HOTSPOTS_COLLECTION_ENABLED env variable or true.
func WithProfilerCodeHotspots(enabled bool) StartOption {
	return func(c *config) {
		c.profilerHotspots = enabled
	}
}

// WithProfilerEndpoints enables the endpoints integration between the tracer
// and profiler. This is done by automatically attaching a pprof label called
// "trace endpoint" holding the resource name of the top-level service span if
// its type is "http", "rpc" or "" (default). You should not use this label
// name in your own code when this is enabled. The enabled value defaults to
// the value of the DD_PROFILING_ENDPOINT_COLLECTION_ENABLED env variable or
// true.
func WithProfilerEndpoints(enabled bool) StartOption {
	return func(c *config) {
		c.profilerEndpoints = enabled
	}
}

// WithDebugSpansMode enables debugging old spans that may have been
// abandoned, which may prevent traces from being set to the Datadog
// Agent, especially if partial flushing is off.
// This setting can also be configured by setting DD_TRACE_DEBUG_ABANDONED_SPANS
// to true. The timeout will default to 10 minutes, unless overwritten
// by DD_TRACE_ABANDONED_SPAN_TIMEOUT.
// This feature is disabled by default. Turning on this debug mode may
// be expensive, so it should only be enabled for debugging purposes.
func WithDebugSpansMode(timeout time.Duration) StartOption {
	return func(c *config) {
		c.debugAbandonedSpans = true
		c.spanTimeout = timeout
	}
}

// WithPartialFlushing enables flushing of partially finished traces.
// This is done after "numSpans" have finished in a single local trace at
// which point all finished spans in that trace will be flushed, freeing up
// any memory they were consuming. This can also be configured by setting
// DD_TRACE_PARTIAL_FLUSH_ENABLED to true, which will default to 1000 spans
// unless overriden with DD_TRACE_PARTIAL_FLUSH_MIN_SPANS. Partial flushing
// is disabled by default.
func WithPartialFlushing(numSpans int) StartOption {
	return func(c *config) {
		c.partialFlushEnabled = true
		c.partialFlushMinSpans = numSpans
	}
}

// WithStatsComputation enables client-side stats computation, allowing
// the tracer to compute stats from traces. This can reduce network traffic
// to the Datadog Agent, and produce more accurate stats data.
// This can also be configured by setting DD_TRACE_STATS_COMPUTATION_ENABLED to true.
// Client-side stats is off by default.
func WithStatsComputation(enabled bool) StartOption {
	return func(c *config) {
		c.statsComputationEnabled = enabled
	}
}

// WithOrchestrion configures Orchestrion's auto-instrumentation metadata.
// This option is only intended to be used by Orchestrion https://github.com/DataDog/orchestrion
func WithOrchestrion(metadata map[string]string) StartOption {
	return func(c *config) {
		c.orchestrionCfg.Enabled = true
		c.orchestrionCfg.Metadata = metadata
	}
}

// StartSpanOption is a configuration option for StartSpan. It is aliased in order
// to help godoc group all the functions returning it together. It is considered
// more correct to refer to it as the type as the origin, ddtrace.StartSpanOption.
type StartSpanOption = ddtrace.StartSpanOption

// Tag sets the given key/value pair as a tag on the started Span.
func Tag(k string, v interface{}) StartSpanOption {
	return func(cfg *ddtrace.StartSpanConfig) {
		if cfg.Tags == nil {
			cfg.Tags = map[string]interface{}{}
		}
		cfg.Tags[k] = v
	}
}

// ServiceName sets the given service name on the started span. For example "http.server".
func ServiceName(name string) StartSpanOption {
	return Tag(ext.ServiceName, name)
}

// ResourceName sets the given resource name on the started span. A resource could
// be an SQL query, a URL, an RPC method or something else.
func ResourceName(name string) StartSpanOption {
	return Tag(ext.ResourceName, name)
}

// SpanType sets the given span type on the started span. Some examples in the case of
// the Datadog APM product could be "web", "db" or "cache".
func SpanType(name string) StartSpanOption {
	return Tag(ext.SpanType, name)
}

// WithSpanLinks sets span links on the started span.
func WithSpanLinks(links []ddtrace.SpanLink) StartSpanOption {
	return func(cfg *ddtrace.StartSpanConfig) {
		cfg.SpanLinks = append(cfg.SpanLinks, links...)
	}
}

var measuredTag = Tag(keyMeasured, 1)

// Measured marks this span to be measured for metrics and stats calculations.
func Measured() StartSpanOption {
	// cache a global instance of this tag: saves one alloc/call
	return measuredTag
}

// WithSpanID sets the SpanID on the started span, instead of using a random number.
// If there is no parent Span (eg from ChildOf), then the TraceID will also be set to the
// value given here.
func WithSpanID(id uint64) StartSpanOption {
	return func(cfg *ddtrace.StartSpanConfig) {
		cfg.SpanID = id
	}
}

// ChildOf tells StartSpan to use the given span context as a parent for the
// created span.
func ChildOf(ctx ddtrace.SpanContext) StartSpanOption {
	return func(cfg *ddtrace.StartSpanConfig) {
		cfg.Parent = ctx
	}
}

// withContext associates the ctx with the span.
func withContext(ctx context.Context) StartSpanOption {
	return func(cfg *ddtrace.StartSpanConfig) {
		cfg.Context = ctx
	}
}

// StartTime sets a custom time as the start time for the created span. By
// default a span is started using the creation time.
func StartTime(t time.Time) StartSpanOption {
	return func(cfg *ddtrace.StartSpanConfig) {
		cfg.StartTime = t
	}
}

// AnalyticsRate sets a custom analytics rate for a span. It decides the percentage
// of events that will be picked up by the App Analytics product. It's represents a
// float64 between 0 and 1 where 0.5 would represent 50% of events.
func AnalyticsRate(rate float64) StartSpanOption {
	if math.IsNaN(rate) {
		return func(cfg *ddtrace.StartSpanConfig) {}
	}
	return Tag(ext.EventSampleRate, rate)
}

// FinishOption is a configuration option for FinishSpan. It is aliased in order
// to help godoc group all the functions returning it together. It is considered
// more correct to refer to it as the type as the origin, ddtrace.FinishOption.
type FinishOption = ddtrace.FinishOption

// FinishTime sets the given time as the finishing time for the span. By default,
// the current time is used.
func FinishTime(t time.Time) FinishOption {
	return func(cfg *ddtrace.FinishConfig) {
		cfg.FinishTime = t
	}
}

// WithError marks the span as having had an error. It uses the information from
// err to set tags such as the error message, error type and stack trace. It has
// no effect if the error is nil.
func WithError(err error) FinishOption {
	return func(cfg *ddtrace.FinishConfig) {
		cfg.Error = err
	}
}

// NoDebugStack prevents any error presented using the WithError finishing option
// from generating a stack trace. This is useful in situations where errors are frequent
// and performance is critical.
func NoDebugStack() FinishOption {
	return func(cfg *ddtrace.FinishConfig) {
		cfg.NoDebugStack = true
	}
}

// StackFrames limits the number of stack frames included into erroneous spans to n, starting from skip.
func StackFrames(n, skip uint) FinishOption {
	if n == 0 {
		return NoDebugStack()
	}
	return func(cfg *ddtrace.FinishConfig) {
		cfg.StackFrames = n
		cfg.SkipStackFrames = skip
	}
}

// WithHeaderTags enables the integration to attach HTTP request headers as span tags.
// Warning:
// Using this feature can risk exposing sensitive data such as authorization tokens to Datadog.
// Special headers can not be sub-selected. E.g., an entire Cookie header would be transmitted, without the ability to choose specific Cookies.
func WithHeaderTags(headerAsTags []string) StartOption {
	return func(c *config) {
		c.headerAsTags = newDynamicConfig("trace_header_tags", headerAsTags, setHeaderTags, equalSlice[string])
		setHeaderTags(headerAsTags)
	}
}

// setHeaderTags sets the global header tags.
// Always resets the global value and returns true.
func setHeaderTags(headerAsTags []string) bool {
	globalconfig.ClearHeaderTags()
	for _, h := range headerAsTags {
		if strings.HasPrefix(h, "x-datadog-") {
			continue
		}
		header, tag := normalizer.HeaderTag(h)
		globalconfig.SetHeaderTag(header, tag)
	}
	return true
}

// WithContribStats opens up a channel of communication between tracer and contrib libraries
// for submitting stats from contribs to Datadog via the tracer's statsd client
// It is enabled by default but can be disabled with `WithContribStats(false)`
func WithContribStats(enabled bool) StartOption {
	return func(c *config) {
		c.contribStats = enabled
	}
}

// UserMonitoringConfig is used to configure what is used to identify a user.
// This configuration can be set by combining one or several UserMonitoringOption with a call to SetUser().
type UserMonitoringConfig struct {
	PropagateID bool
	Email       string
	Name        string
	Role        string
	SessionID   string
	Scope       string
	Metadata    map[string]string
}

// UserMonitoringOption represents a function that can be provided as a parameter to SetUser.
type UserMonitoringOption func(*UserMonitoringConfig)

// WithUserMetadata returns the option setting additional metadata of the authenticated user.
// This can be used multiple times and the given data will be tracked as `usr.{key}=value`.
func WithUserMetadata(key, value string) UserMonitoringOption {
	return func(cfg *UserMonitoringConfig) {
		cfg.Metadata[key] = value
	}
}

// WithUserEmail returns the option setting the email of the authenticated user.
func WithUserEmail(email string) UserMonitoringOption {
	return func(cfg *UserMonitoringConfig) {
		cfg.Email = email
	}
}

// WithUserName returns the option setting the name of the authenticated user.
func WithUserName(name string) UserMonitoringOption {
	return func(cfg *UserMonitoringConfig) {
		cfg.Name = name
	}
}

// WithUserSessionID returns the option setting the session ID of the authenticated user.
func WithUserSessionID(sessionID string) UserMonitoringOption {
	return func(cfg *UserMonitoringConfig) {
		cfg.SessionID = sessionID
	}
}

// WithUserRole returns the option setting the role of the authenticated user.
func WithUserRole(role string) UserMonitoringOption {
	return func(cfg *UserMonitoringConfig) {
		cfg.Role = role
	}
}

// WithUserScope returns the option setting the scope (authorizations) of the authenticated user.
func WithUserScope(scope string) UserMonitoringOption {
	return func(cfg *UserMonitoringConfig) {
		cfg.Scope = scope
	}
}

// WithPropagation returns the option allowing the user id to be propagated through distributed traces.
// The user id is base64 encoded and added to the datadog propagated tags header.
// This option should only be used if you are certain that the user id passed to `SetUser()` does not contain any
// personal identifiable information or any kind of sensitive data, as it will be leaked to other services.
func WithPropagation() UserMonitoringOption {
	return func(cfg *UserMonitoringConfig) {
		cfg.PropagateID = true
	}
}
