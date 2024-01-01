// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

package sharedsec

import (
	_ "embed" // Blank import
	"errors"
	"net/http"
	"os"
	"strings"

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
}

type (
	// Action represents a WAF action.
	// It holds the HTTP and gRPC handlers to be used instead of the regular
	// request handler when said action is executed.
	Action struct {
		http     http.Handler
		grpc     GRPCWrapper
		blocking bool
	}

	// GRPCWrapper is an opaque prototype abstraction for a gRPC handler (to avoid importing grpc)
	// that takes metadata as input and returns a status code and an error
	// TODO: rely on strongly typed actions (with the actual grpc types) by introducing WAF constructors
	//     living in the contrib packages, along with their dependencies - something like `appsec.RegisterWAFConstructor(newGRPCWAF)`
	//    Such constructors would receive the full appsec config and rules, so that they would be able to build
	//    specific blocking actions.
	GRPCWrapper func(map[string][]string) (uint32, error)
)

// Blocking returns true if the action object represents a request blocking action
func (a *Action) Blocking() bool {
	return a.blocking
}

// NewBlockHandler creates, initializes and returns a new BlockRequestAction
func NewBlockHandler(status int, template string) http.Handler {
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		w.Write(payload)
	})
}

func newGRPCBlockHandler(status int) GRPCWrapper {
	return func(_ map[string][]string) (uint32, error) {
		return uint32(status), errors.New("Request blocked")
	}
}

// NewBlockRequestAction creates an action for the "block" action type
func NewBlockRequestAction(httpStatus, grpcStatus int, template string) *Action {
	return &Action{
		http:     NewBlockHandler(httpStatus, template),
		grpc:     newGRPCBlockHandler(grpcStatus),
		blocking: true,
	}
}

// NewRedirectRequestAction creates an action for the "redirect" action type
func NewRedirectRequestAction(status int, loc string) *Action {
	return &Action{
		http: http.RedirectHandler(loc, status),
		// gRPC is not handled by our SRB RFCs so far
		// Use the default block handler for now
		grpc: newGRPCBlockHandler(10),
	}
}

// HTTP returns the HTTP handler linked to the action object
func (a *Action) HTTP() http.Handler {
	return a.http
}

// GRPC returns the gRPC handler linked to the action object
func (a *Action) GRPC() GRPCWrapper {
	return a.grpc
}
