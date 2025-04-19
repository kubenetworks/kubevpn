// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package actions

import (
	_ "embed" // embed is used to embed the blocked-template.json and blocked-template.html files
	"net/http"
	"os"
	"strings"

	"github.com/mitchellh/mapstructure"

	"gopkg.in/DataDog/dd-trace-go.v1/appsec/events"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

// blockedTemplateJSON is the default JSON template used to write responses for blocked requests
//
//go:embed blocked-template.json
var blockedTemplateJSON []byte

// blockedTemplateHTML is the default HTML template used to write responses for blocked requests
//
//go:embed blocked-template.html
var blockedTemplateHTML []byte

const (
	envBlockedTemplateHTML = "DD_APPSEC_HTTP_BLOCKED_TEMPLATE_HTML"
	envBlockedTemplateJSON = "DD_APPSEC_HTTP_BLOCKED_TEMPLATE_JSON"
)

func init() {
	for env, template := range map[string]*[]byte{envBlockedTemplateJSON: &blockedTemplateJSON, envBlockedTemplateHTML: &blockedTemplateHTML} {
		if path, ok := os.LookupEnv(env); ok {
			if t, err := os.ReadFile(path); err != nil {
				log.Error("Could not read template at %s: %v", path, err)
			} else {
				*template = t
			}
		}
	}

	registerActionHandler("block_request", NewBlockAction)
}

type (
	// blockActionParams are the dynamic parameters to be provided to a "block_request"
	// action type upon invocation
	blockActionParams struct {
		// GRPCStatusCode is the gRPC status code to be returned. Since 0 is the OK status, the value is nullable to
		// be able to distinguish between unset and defaulting to Abort (10), or set to OK (0).
		GRPCStatusCode *int   `mapstructure:"grpc_status_code,omitempty"`
		StatusCode     int    `mapstructure:"status_code"`
		Type           string `mapstructure:"type,omitempty"`
	}
	// GRPCWrapper is an opaque prototype abstraction for a gRPC handler (to avoid importing grpc)
	// that returns a status code and an error
	GRPCWrapper func() (uint32, error)

	// BlockGRPC are actions that interact with a GRPC request flow
	BlockGRPC struct {
		GRPCWrapper
	}

	// BlockHTTP are actions that interact with an HTTP request flow
	BlockHTTP struct {
		http.Handler
	}
)

func (a *BlockGRPC) EmitData(op dyngo.Operation) {
	dyngo.EmitData(op, a)
	dyngo.EmitData(op, &events.BlockingSecurityEvent{})
}

func (a *BlockHTTP) EmitData(op dyngo.Operation) {
	dyngo.EmitData(op, a)
	dyngo.EmitData(op, &events.BlockingSecurityEvent{})
}

func newGRPCBlockRequestAction(status int) *BlockGRPC {
	return &BlockGRPC{GRPCWrapper: newGRPCBlockHandler(status)}
}

func newGRPCBlockHandler(status int) GRPCWrapper {
	return func() (uint32, error) {
		return uint32(status), &events.BlockingSecurityEvent{}
	}
}

func blockParamsFromMap(params map[string]any) (blockActionParams, error) {
	grpcCode := 10
	p := blockActionParams{
		Type:           "auto",
		StatusCode:     403,
		GRPCStatusCode: &grpcCode,
	}

	if err := mapstructure.WeakDecode(params, &p); err != nil {
		return p, err
	}

	if p.GRPCStatusCode == nil {
		p.GRPCStatusCode = &grpcCode
	}

	return p, nil
}

// NewBlockAction creates an action for the "block_request" action type
func NewBlockAction(params map[string]any) []Action {
	p, err := blockParamsFromMap(params)
	if err != nil {
		log.Debug("appsec: couldn't decode redirect action parameters")
		return nil
	}
	return []Action{
		newHTTPBlockRequestAction(p.StatusCode, p.Type),
		newGRPCBlockRequestAction(*p.GRPCStatusCode),
	}
}

func newHTTPBlockRequestAction(status int, template string) *BlockHTTP {
	return &BlockHTTP{Handler: newBlockHandler(status, template)}
}

// newBlockHandler creates, initializes and returns a new BlockRequestAction
func newBlockHandler(status int, template string) http.Handler {
	htmlHandler := newBlockRequestHandler(status, "text/html", blockedTemplateHTML)
	jsonHandler := newBlockRequestHandler(status, "application/json", blockedTemplateJSON)
	switch template {
	case "json":
		return jsonHandler
	case "html":
		return htmlHandler
	default:
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := jsonHandler
			hdr := r.Header.Get("Accept")
			htmlIdx := strings.Index(hdr, "text/html")
			jsonIdx := strings.Index(hdr, "application/json")
			// Switch to html handler if text/html comes before application/json in the Accept header
			if htmlIdx != -1 && (jsonIdx == -1 || htmlIdx < jsonIdx) {
				h = htmlHandler
			}
			h.ServeHTTP(w, r)
		})
	}
}

func newBlockRequestHandler(status int, ct string, payload []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		w.Write(payload)
	})
}
