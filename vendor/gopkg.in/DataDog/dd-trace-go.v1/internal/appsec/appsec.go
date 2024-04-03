// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package appsec

import (
	"fmt"
	"sync"

	"github.com/DataDog/appsec-internal-go/limiter"
	appsecLog "github.com/DataDog/appsec-internal-go/log"
	waf "github.com/DataDog/go-libddwaf/v2"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/config"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

// Enabled returns true when AppSec is up and running. Meaning that the appsec build tag is enabled, the env var
// DD_APPSEC_ENABLED is set to true, and the tracer is started.
func Enabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	return activeAppSec != nil && activeAppSec.started
}

// Start AppSec when enabled is enabled by both using the appsec build tag and
// setting the environment variable DD_APPSEC_ENABLED to true.
func Start(opts ...config.StartOption) {
	telemetry := newAppsecTelemetry()
	defer telemetry.emit()

	// AppSec can start either:
	// 1. Manually thanks to DD_APPSEC_ENABLED
	// 2. Remotely when DD_APPSEC_ENABLED is undefined
	// Note: DD_APPSEC_ENABLED=false takes precedence over remote configuration
	// and enforces to have AppSec disabled.
	enabled, set, err := config.IsEnabled()
	if err != nil {
		logUnexpectedStartError(err)
		return
	}
	if set {
		telemetry.addEnvConfig("DD_APPSEC_ENABLED", enabled)
	}

	// Check if AppSec is explicitly disabled
	if set && !enabled {
		log.Debug("appsec: disabled by the configuration: set the environment variable DD_APPSEC_ENABLED to true to enable it")
		return
	}

	// Check whether libddwaf - required for Threats Detection - is ok or not
	if ok, err := waf.Health(); !ok {
		// We need to avoid logging an error to APM tracing users who don't necessarily intend to enable appsec
		if set {
			// DD_APPSEC_ENABLED is explicitly set so we log an error
			log.Error("appsec: threats detection cannot be enabled for the following reasons: %vappsec: no security activities will be collected. Please contact support at https://docs.datadoghq.com/help/ for help.", err)
		} else {
			// DD_APPSEC_ENABLED is not set so we cannot know what the intent is here, we must log a
			// debug message instead to avoid showing an error to APM-tracing-only users.
			log.Debug("appsec: remote activation of threats detection cannot be enabled for the following reasons: %v", err)
		}
		return
	}

	// From this point we know that AppSec is either enabled or can be enabled through remote config
	cfg, err := config.NewConfig()
	if err != nil {
		logUnexpectedStartError(err)
		return
	}
	for _, opt := range opts {
		opt(cfg)
	}
	appsec := newAppSec(cfg)

	// Start the remote configuration client
	log.Debug("appsec: starting the remote configuration client")
	if err := appsec.startRC(); err != nil {
		log.Error("appsec: Remote config: disabled due to an instanciation error: %v", err)
	}

	if !set {
		// AppSec is not enforced by the env var and can be enabled through remote config
		log.Debug("appsec: %s is not set, appsec won't start until activated through remote configuration", config.EnvEnabled)
		if err := appsec.enableRemoteActivation(); err != nil {
			// ASM is not enabled and can't be enabled through remote configuration. Nothing more can be done.
			logUnexpectedStartError(err)
			appsec.stopRC()
			return
		}
		log.Debug("appsec: awaiting for possible remote activation")
	} else if err := appsec.start(telemetry); err != nil { // AppSec is specifically enabled
		logUnexpectedStartError(err)
		appsec.stopRC()
		return
	}
	setActiveAppSec(appsec)
}

// Implement the AppSec log message C1
func logUnexpectedStartError(err error) {
	log.Error("appsec: could not start because of an unexpected error: %v\nNo security activities will be collected. Please contact support at https://docs.datadoghq.com/help/ for help.", err)
}

// Stop AppSec.
func Stop() {
	setActiveAppSec(nil)
}

var (
	activeAppSec *appsec
	mu           sync.RWMutex
)

func setActiveAppSec(a *appsec) {
	mu.Lock()
	defer mu.Unlock()
	if activeAppSec != nil {
		activeAppSec.stopRC()
		activeAppSec.stop()
	}
	activeAppSec = a
}

type appsec struct {
	cfg       *config.Config
	limiter   *limiter.TokenTicker
	wafHandle *wafHandle
	started   bool
}

func newAppSec(cfg *config.Config) *appsec {
	return &appsec{
		cfg: cfg,
	}
}

// Start AppSec by registering its security protections according to the configured the security rules.
func (a *appsec) start(telemetry *appsecTelemetry) error {
	// Load the waf to catch early errors if any
	if ok, err := waf.Load(); err != nil {
		// 1. If there is an error and the loading is not ok: log as an unexpected error case and quit appsec
		// Note that we assume here that the test for the unsupported target has been done before calling
		// this method, so it is now considered an error for this method
		if !ok {
			return fmt.Errorf("error while loading libddwaf: %w", err)
		}
		// 2. If there is an error and the loading is ok: log as an informative error where appsec can be used
		log.Error("appsec: non-critical error while loading libddwaf: %v", err)
	}

	a.limiter = limiter.NewTokenTicker(a.cfg.TraceRateLimit, a.cfg.TraceRateLimit)
	a.limiter.Start()

	// Register the WAF operation event listener
	if err := a.swapWAF(a.cfg.RulesManager.Latest); err != nil {
		return err
	}

	a.enableRCBlocking()

	a.started = true
	log.Info("appsec: up and running")

	// TODO: log the config like the APM tracer does but we first need to define
	//   an user-friendly string representation of our config and its sources

	telemetry.setEnabled()
	return nil
}

// Stop AppSec by unregistering the security protections.
func (a *appsec) stop() {
	if !a.started {
		return
	}
	telemetry := newAppsecTelemetry()
	defer telemetry.emit()

	a.started = false
	// Disable RC blocking first so that the following is guaranteed not to be concurrent anymore.
	a.disableRCBlocking()

	// Disable the currently applied instrumentation
	dyngo.SwapRootOperation(nil)
	if a.wafHandle != nil {
		a.wafHandle.Close()
		a.wafHandle = nil
	}
	// TODO: block until no more requests are using dyngo operations

	a.limiter.Stop()
}

func init() {
	appsecLog.SetBackend(appsecLog.Backend{
		Debug: log.Debug,
		Info:  log.Info,
		Warn:  log.Warn,
		Errorf: func(s string, a ...any) error {
			err := fmt.Errorf(s, a...)
			log.Error(err.Error())
			return err
		},
	})
}
