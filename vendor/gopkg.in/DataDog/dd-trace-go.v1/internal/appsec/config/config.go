// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	internal "github.com/DataDog/appsec-internal-go/appsec"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/remoteconfig"
)

// EnvEnabled is the env var used to enable/disable appsec
const EnvEnabled = "DD_APPSEC_ENABLED"

// StartOption is used to customize the AppSec configuration when invoked with appsec.Start()
type StartOption func(c *Config)

// Config is the AppSec configuration.
type Config struct {
	// rules loaded via the env var DD_APPSEC_RULES. When not set, the builtin rules will be used
	// and live-updated with remote configuration.
	RulesManager *RulesManager
	// Maximum WAF execution time
	WAFTimeout time.Duration
	// AppSec trace rate limit (traces per second).
	TraceRateLimit int64
	// Obfuscator configuration
	Obfuscator internal.ObfuscatorConfig
	// APISec configuration
	APISec internal.APISecConfig
	// RC is the remote configuration client used to receive product configuration updates. Nil if RC is disabled (default)
	RC *remoteconfig.ClientConfig
}

// WithRCConfig sets the AppSec remote config client configuration to the specified cfg
func WithRCConfig(cfg remoteconfig.ClientConfig) StartOption {
	return func(c *Config) {
		c.RC = &cfg
	}
}

// IsEnabled returns true when appsec is enabled when the environment variable
// DD_APPSEC_ENABLED is set to true.
// It also returns whether the env var is actually set in the env or not.
func IsEnabled() (enabled bool, set bool, err error) {
	enabledStr, set := os.LookupEnv(EnvEnabled)
	if enabledStr == "" {
		return false, set, nil
	} else if enabled, err = strconv.ParseBool(enabledStr); err != nil {
		return false, set, fmt.Errorf("could not parse %s value `%s` as a boolean value", EnvEnabled, enabledStr)
	}

	return enabled, set, nil
}

// NewConfig returns a fresh appsec configuration read from the env
func NewConfig() (*Config, error) {
	rules, err := internal.RulesFromEnv()
	if err != nil {
		return nil, err
	}

	r, err := NewRulesManeger(rules)
	if err != nil {
		return nil, err
	}

	return &Config{
		RulesManager:   r,
		WAFTimeout:     internal.WAFTimeoutFromEnv(),
		TraceRateLimit: int64(internal.RateLimitFromEnv()),
		Obfuscator:     internal.NewObfuscatorConfig(),
		APISec:         internal.NewAPISecConfig(),
	}, nil
}
