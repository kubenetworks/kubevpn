// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package actions

import (
	"net/http"

	"github.com/mitchellh/mapstructure"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

// redirectActionParams are the dynamic parameters to be provided to a "redirect_request"
// action type upon invocation
type redirectActionParams struct {
	Location   string `mapstructure:"location,omitempty"`
	StatusCode int    `mapstructure:"status_code"`
}

func init() {
	registerActionHandler("redirect_request", NewRedirectAction)
}

func redirectParamsFromMap(params map[string]any) (redirectActionParams, error) {
	var p redirectActionParams
	err := mapstructure.WeakDecode(params, &p)
	return p, err
}

func newRedirectRequestAction(status int, loc string) *BlockHTTP {
	// Default to 303 if status is out of redirection codes bounds
	if status < http.StatusMultipleChoices || status >= http.StatusBadRequest {
		status = http.StatusSeeOther
	}

	// If location is not set we fall back on a default block action
	if loc == "" {
		return &BlockHTTP{Handler: newBlockHandler(http.StatusForbidden, string(blockedTemplateJSON))}
	}
	return &BlockHTTP{Handler: http.RedirectHandler(loc, status)}
}

// NewRedirectAction creates an action for the "redirect_request" action type
func NewRedirectAction(params map[string]any) []Action {
	p, err := redirectParamsFromMap(params)
	if err != nil {
		log.Debug("appsec: couldn't decode redirect action parameters")
		return nil
	}
	return []Action{newRedirectRequestAction(p.StatusCode, p.Location)}
}
