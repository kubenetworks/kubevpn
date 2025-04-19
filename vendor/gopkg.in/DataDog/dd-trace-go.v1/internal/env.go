// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package internal

import (
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

// BoolEnv returns the parsed boolean value of an environment variable, or
// def otherwise.
func BoolEnv(key string, def bool) bool {
	vv, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	v, err := strconv.ParseBool(vv)
	if err != nil {
		log.Warn("Non-boolean value for env var %s, defaulting to %t. Parse failed with error: %v", key, def, err)
		return def
	}
	return v
}

// IntEnv returns the parsed int value of an environment variable, or
// def otherwise.
func IntEnv(key string, def int) int {
	vv, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	v, err := strconv.Atoi(vv)
	if err != nil {
		log.Warn("Non-integer value for env var %s, defaulting to %d. Parse failed with error: %v", key, def, err)
		return def
	}
	return v
}

// DurationEnv returns the parsed duration value of an environment variable, or
// def otherwise.
func DurationEnv(key string, def time.Duration) time.Duration {
	vv, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	v, err := time.ParseDuration(vv)
	if err != nil {
		log.Warn("Non-duration value for env var %s, defaulting to %d. Parse failed with error: %v", key, def, err)
		return def
	}
	return v
}

// IpEnv returns the valid IP value of an environment variable, or def otherwise.
func IpEnv(key string, def net.IP) net.IP {
	vv, ok := os.LookupEnv(key)
	if !ok {
		return def
	}

	ip := net.ParseIP(vv)
	if ip == nil {
		log.Warn("Non-IP value for env var %s, defaulting to %s", key, def.String())
		return def
	}

	return ip
}

// ForEachStringTag runs fn on every key val pair encountered in str.
// str may contain multiple key val pairs separated by either space
// or comma (but not a mixture of both), and each key val pair is separated by a delimiter.
func ForEachStringTag(str string, delimiter string, fn func(key string, val string)) {
	sep := " "
	if strings.Index(str, ",") > -1 {
		// falling back to comma as separator
		sep = ","
	}
	for _, tag := range strings.Split(str, sep) {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		kv := strings.SplitN(tag, delimiter, 2)
		key := strings.TrimSpace(kv[0])
		if key == "" {
			continue
		}
		var val string
		if len(kv) == 2 {
			val = strings.TrimSpace(kv[1])
		}
		fn(key, val)
	}
}

// ParseTagString returns tags parsed from string as map
func ParseTagString(str string) map[string]string {
	res := make(map[string]string)
	ForEachStringTag(str, DDTagsDelimiter, func(key, val string) { res[key] = val })
	return res
}

// FloatEnv returns the parsed float64 value of an environment variable,
// or def otherwise.
func FloatEnv(key string, def float64) float64 {
	env, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	v, err := strconv.ParseFloat(env, 64)
	if err != nil {
		log.Warn("Non-float value for env var %s, defaulting to %f. Parse failed with error: %v", key, def, err)
		return def
	}
	return v
}

// BoolVal returns the parsed boolean value of string val, or def if not parseable
func BoolVal(val string, def bool) bool {
	v, err := strconv.ParseBool(val)
	if err != nil {
		return def
	}
	return v
}

// ExternalEnvironment returns the value of the DD_EXTERNAL_ENV environment variable.
func ExternalEnvironment() string {
	return os.Getenv("DD_EXTERNAL_ENV")
}
