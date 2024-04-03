// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package appsec

import (
	"github.com/DataDog/appsec-internal-go/limiter"
	waf "github.com/DataDog/go-libddwaf/v2"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/config"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter/sharedsec"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

type wafHandle struct {
	*waf.Handle
	// actions are tightly link to a ruleset, which is linked to a waf handle
	actions sharedsec.Actions
}

func (a *appsec) swapWAF(rules config.RulesFragment) (err error) {
	// Instantiate a new WAF handle and verify its state
	newHandle, err := newWAFHandle(rules, a.cfg)
	if err != nil {
		return err
	}

	// Close the WAF handle in case of an error in what's following
	defer func() {
		if err != nil {
			newHandle.Close()
		}
	}()

	newRoot := dyngo.NewRootOperation()
	for _, fn := range wafEventListeners {
		fn(newHandle.Handle, newHandle.actions, a.cfg, a.limiter, newRoot)
	}

	// Hot-swap dyngo's root operation
	dyngo.SwapRootOperation(newRoot)

	// Close old handle.
	// Note that concurrent requests are still using it, and it will be released
	// only when no more requests use it.
	// TODO: implement in dyngo ref-counting of the root operation so we can
	//   rely on a Finish event listener on the root operation instead?
	//   Avoiding saving the current WAF handle would guarantee no one is
	//   accessing a.wafHandle while we swap
	oldHandle := a.wafHandle
	a.wafHandle = newHandle
	if oldHandle != nil {
		oldHandle.Close()
	}

	return nil
}

func actionFromEntry(e *config.ActionEntry) *sharedsec.Action {
	switch e.Type {
	case "block_request":
		grpcCode := 10 // use the grpc.Codes value for "Aborted" by default
		if e.Parameters.GRPCStatusCode != nil {
			grpcCode = *e.Parameters.GRPCStatusCode
		}
		return sharedsec.NewBlockRequestAction(e.Parameters.StatusCode, grpcCode, e.Parameters.Type)
	case "redirect_request":
		return sharedsec.NewRedirectRequestAction(e.Parameters.StatusCode, e.Parameters.Location)
	default:
		log.Debug("appsec: unknown action type `%s`", e.Type)
		return nil
	}
}

func newWAFHandle(rules config.RulesFragment, cfg *config.Config) (*wafHandle, error) {
	handle, err := waf.NewHandle(rules, cfg.Obfuscator.KeyRegex, cfg.Obfuscator.ValueRegex)
	actions := sharedsec.Actions{
		// Default built-in block action
		"block": sharedsec.NewBlockRequestAction(403, 10, "auto"),
	}

	for _, entry := range rules.Actions {
		a := actionFromEntry(&entry)
		if a != nil {
			actions[entry.ID] = a
		}
	}
	return &wafHandle{
		Handle:  handle,
		actions: actions,
	}, err
}

type wafEventListener func(*waf.Handle, sharedsec.Actions, *config.Config, limiter.Limiter, dyngo.Operation)

// wafEventListeners is the global list of event listeners registered by contribs at init time. This
// is thread-safe assuming all writes (via AddWAFEventListener) are performed within `init`
// functions; so this is written to only during initialization, and is read from concurrently only
// during runtime when no writes are happening anymore.
var wafEventListeners []wafEventListener

// AddWAFEventListener adds a new WAF event listener to be registered whenever a new root operation
// is created. The normal way to use this is to call it from a `func init() {}` so that it is
// guaranteed to have happened before any listened to event may be emitted.
func AddWAFEventListener(fn wafEventListener) {
	wafEventListeners = append(wafEventListeners, fn)
}
