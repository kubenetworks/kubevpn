// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023-present Datadog, Inc.

package appsec

import (
	"os"
	"regexp"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/DataDog/appsec-internal-go/log"
)

// Configuration environment variables
const (
	// EnvAPISecEnabled is the env var used to enable API Security
	EnvAPISecEnabled = "DD_API_SECURITY_ENABLED"
	// EnvAPISecSampleRate is the env var used to set the sampling rate of API Security schema extraction
	EnvAPISecSampleRate = "DD_API_SECURITY_REQUEST_SAMPLE_RATE"
	// EnvObfuscatorKey is the env var used to provide the WAF key obfuscation regexp
	EnvObfuscatorKey = "DD_APPSEC_OBFUSCATION_PARAMETER_KEY_REGEXP"
	// EnvObfuscatorValue is the env var used to provide the WAF value obfuscation regexp
	EnvObfuscatorValue = "DD_APPSEC_OBFUSCATION_PARAMETER_VALUE_REGEXP"
	// EnvWAFTimeout is the env var used to specify the timeout value for a WAF run
	EnvWAFTimeout = "DD_APPSEC_WAF_TIMEOUT"
	// EnvTraceRateLimit is the env var used to set the ASM trace limiting rate
	EnvTraceRateLimit = "DD_APPSEC_TRACE_RATE_LIMIT"
	// EnvRules is the env var used to provide a path to a local security rule file
	EnvRules = "DD_APPSEC_RULES"
)

// Configuration constants and default values
const (
	// DefaultAPISecSampleRate is the default rate at which API Security schemas are extracted from requests
	DefaultAPISecSampleRate = .1
	// DefaultObfuscatorKeyRegex is the default regexp used to obfuscate keys
	DefaultObfuscatorKeyRegex = `(?i)(?:p(?:ass)?w(?:or)?d|pass(?:_?phrase)?|secret|(?:api_?|private_?|public_?)key)|token|consumer_?(?:id|key|secret)|sign(?:ed|ature)|bearer|authorization`
	// DefaultObfuscatorValueRegex is the default regexp used to obfuscate values
	DefaultObfuscatorValueRegex = `(?i)(?:p(?:ass)?w(?:or)?d|pass(?:_?phrase)?|secret|(?:api_?|private_?|public_?|access_?|secret_?)key(?:_?id)?|token|consumer_?(?:id|key|secret)|sign(?:ed|ature)?|auth(?:entication|orization)?)(?:\s*=[^;]|"\s*:\s*"[^"]+")|bearer\s+[a-z0-9\._\-]+|token:[a-z0-9]{13}|gh[opsu]_[0-9a-zA-Z]{36}|ey[I-L][\w=-]+\.ey[I-L][\w=-]+(?:\.[\w.+\/=-]+)?|[\-]{5}BEGIN[a-z\s]+PRIVATE\sKEY[\-]{5}[^\-]+[\-]{5}END[a-z\s]+PRIVATE\sKEY|ssh-rsa\s*[a-z0-9\/\.+]{100,}`
	// DefaultWAFTimeout is the default time limit past which a WAF run will timeout
	DefaultWAFTimeout = time.Millisecond
	// DefaultTraceRate is the default limit (trace/sec) past which ASM traces are sampled out
	DefaultTraceRate uint = 100 // up to 100 appsec traces/s
)

// APISecConfig holds the configuration for API Security schemas reporting
// It is used to enabled/disable the feature as well as to configure the rate
// at which schemas get reported,
type APISecConfig struct {
	Enabled    bool
	SampleRate float64
}

// ObfuscatorConfig wraps the key and value regexp to be passed to the WAF to perform obfuscation.
type ObfuscatorConfig struct {
	KeyRegex   string
	ValueRegex string
}

// NewAPISecConfig creates and returns a new API Security configuration by reading the env
func NewAPISecConfig() APISecConfig {
	return APISecConfig{
		Enabled:    apiSecurityEnabled(),
		SampleRate: readAPISecuritySampleRate(),
	}
}

func apiSecurityEnabled() bool {
	enabled := true
	str, set := os.LookupEnv(EnvAPISecEnabled)
	if set {
		var err error
		enabled, err = strconv.ParseBool(str)
		if err != nil {
			logEnvVarParsingError(EnvAPISecEnabled, str, err, enabled)
		}
	}
	return enabled
}

func readAPISecuritySampleRate() float64 {
	value := os.Getenv(EnvAPISecSampleRate)
	rate, err := strconv.ParseFloat(value, 64)
	if err != nil {
		logEnvVarParsingError(EnvAPISecSampleRate, value, err, DefaultAPISecSampleRate)
		return DefaultAPISecSampleRate
	}
	// Clamp the value so that 0.0 <= rate <= 1.0
	if rate < 0. {
		rate = 0.
	} else if rate > 1. {
		rate = 1.
	}
	return rate
}

// NewObfuscatorConfig creates and returns a new WAF obfuscator configuration by reading the env
func NewObfuscatorConfig() ObfuscatorConfig {
	keyRE := readObfuscatorConfigRegexp(EnvObfuscatorKey, DefaultObfuscatorKeyRegex)
	valueRE := readObfuscatorConfigRegexp(EnvObfuscatorValue, DefaultObfuscatorValueRegex)
	return ObfuscatorConfig{KeyRegex: keyRE, ValueRegex: valueRE}
}

func readObfuscatorConfigRegexp(name, defaultValue string) string {
	val, present := os.LookupEnv(name)
	if !present {
		log.Debug("appsec: %s not defined, starting with the default obfuscator regular expression", name)
		return defaultValue
	}
	if _, err := regexp.Compile(val); err != nil {
		logUnexpectedEnvVarValue(name, val, "could not compile the configured obfuscator regular expression", defaultValue)
		return defaultValue
	}
	log.Debug("appsec: starting with the configured obfuscator regular expression %s", name)
	return val
}

// WAFTimeoutFromEnv reads and parses the WAF timeout value set through the env
// If not set, it defaults to `DefaultWAFTimeout`
func WAFTimeoutFromEnv() (timeout time.Duration) {
	timeout = DefaultWAFTimeout
	value := os.Getenv(EnvWAFTimeout)
	if value == "" {
		return
	}

	// Check if the value ends with a letter, which means the user has
	// specified their own time duration unit(s) such as 1s200ms.
	// Otherwise, default to microseconds.
	if lastRune, _ := utf8.DecodeLastRuneInString(value); !unicode.IsLetter(lastRune) {
		value += "us" // Add the default microsecond time-duration suffix
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		logEnvVarParsingError(EnvWAFTimeout, value, err, timeout)
		return
	}
	if parsed <= 0 {
		logUnexpectedEnvVarValue(EnvWAFTimeout, parsed, "expecting a strictly positive duration", timeout)
		return
	}
	return parsed
}

// RateLimitFromEnv reads and parses the trace rate limit set through the env
// If not set, it defaults to `DefaultTraceRate`
func RateLimitFromEnv() (rate uint) {
	rate = DefaultTraceRate
	value := os.Getenv(EnvTraceRateLimit)
	if value == "" {
		return rate
	}
	parsed, err := strconv.ParseUint(value, 10, 0)
	if err != nil {
		logEnvVarParsingError(EnvTraceRateLimit, value, err, rate)
		return
	}
	if parsed == 0 {
		logUnexpectedEnvVarValue(EnvTraceRateLimit, parsed, "expecting a value strictly greater than 0", rate)
		return
	}
	return uint(parsed)
}

// RulesFromEnv returns the security rules provided through the environment
// If the env var is not set, the default recommended rules are returned instead
func RulesFromEnv() ([]byte, error) {
	filepath := os.Getenv(EnvRules)
	if filepath == "" {
		log.Debug("appsec: using the default built-in recommended security rules")
		return DefaultRuleset()
	}
	buf, err := os.ReadFile(filepath)
	if err != nil {
		if os.IsNotExist(err) {
			err = log.Errorf("appsec: could not find the rules file in path %s: %w.", filepath, err)
		}
		return nil, err
	}
	log.Debug("appsec: using the security rules from file %s", filepath)
	return buf, nil
}

func logEnvVarParsingError(name, value string, err error, defaultValue any) {
	log.Debug("appsec: could not parse the env var %s=%s as a duration: %v. Using default value %v.", name, value, err, defaultValue)
}

func logUnexpectedEnvVarValue(name string, value any, reason string, defaultValue any) {
	log.Debug("appsec: unexpected configuration value of %s=%v: %s. Using default value %v.", name, value, reason, defaultValue)
}
