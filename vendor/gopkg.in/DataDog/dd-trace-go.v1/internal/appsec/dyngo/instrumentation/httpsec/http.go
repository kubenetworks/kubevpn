// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

// Package httpsec defines is the HTTP instrumentation API and contract for
// AppSec. It defines an abstract representation of HTTP handlers, along with
// helper functions to wrap (aka. instrument) standard net/http handlers.
// HTTP integrations must use this package to enable AppSec features for HTTP,
// which listens to this package's operation events.
package httpsec

import (
	"context"
	// Blank import needed to use embed for the default blocked response payloads
	_ "embed"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo/instrumentation"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo/instrumentation/sharedsec"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"

	"github.com/DataDog/appsec-internal-go/netip"
)

// Abstract HTTP handler operation definition.
type (
	// HandlerOperationArgs is the HTTP handler operation arguments.
	HandlerOperationArgs struct {
		// Method is the http method verb of the request, address is `server.request.method`
		Method string
		// RequestURI corresponds to the address `server.request.uri.raw`
		RequestURI string
		// Headers corresponds to the address `server.request.headers.no_cookies`
		Headers map[string][]string
		// Cookies corresponds to the address `server.request.cookies`
		Cookies map[string][]string
		// Query corresponds to the address `server.request.query`
		Query map[string][]string
		// PathParams corresponds to the address `server.request.path_params`
		PathParams map[string]string
		// ClientIP corresponds to the address `http.client_ip`
		ClientIP netip.Addr
	}

	// HandlerOperationRes is the HTTP handler operation results.
	HandlerOperationRes struct {
		// Status corresponds to the address `server.response.status`.
		Status int
	}

	// SDKBodyOperationArgs is the SDK body operation arguments.
	SDKBodyOperationArgs struct {
		// Body corresponds to the address `server.request.body`.
		Body interface{}
	}

	// SDKBodyOperationRes is the SDK body operation results.
	SDKBodyOperationRes struct{}

	// MonitoringError is used to vehicle an HTTP error, usually resurfaced through Appsec SDKs.
	MonitoringError struct {
		msg string
	}
)

// Error implements the Error interface
func (e *MonitoringError) Error() string {
	return e.msg
}

// NewMonitoringError creates and returns a new HTTP monitoring error, wrapped under
// sharedesec.MonitoringError
func NewMonitoringError(msg string) error {
	return &MonitoringError{
		msg: msg,
	}
}

// MonitorParsedBody starts and finishes the SDK body operation.
// This function should not be called when AppSec is disabled in order to
// get preciser error logs.
func MonitorParsedBody(ctx context.Context, body interface{}) error {
	parent := fromContext(ctx)
	if parent == nil {
		log.Error("appsec: parsed http body monitoring ignored: could not find the http handler instrumentation metadata in the request context: the request handler is not being monitored by a middleware function or the provided context is not the expected request context")
		return nil
	}

	return ExecuteSDKBodyOperation(parent, SDKBodyOperationArgs{Body: body})
}

// ExecuteSDKBodyOperation starts and finishes the SDK Body operation by emitting a dyngo start and finish events
// An error is returned if the body associated to that operation must be blocked
func ExecuteSDKBodyOperation(parent dyngo.Operation, args SDKBodyOperationArgs) error {
	var err error
	op := &SDKBodyOperation{Operation: dyngo.NewOperation(parent)}
	sharedsec.OnErrorData(op, func(e error) {
		err = e
	})
	dyngo.StartOperation(op, args)
	dyngo.FinishOperation(op, SDKBodyOperationRes{})
	return err
}

// WrapHandler wraps the given HTTP handler with the abstract HTTP operation defined by HandlerOperationArgs and
// HandlerOperationRes.
// The onBlock params are used to cleanup the context when needed.
// It is a specific patch meant for Gin, for which we must abort the
// context since it uses a queue of handlers and it's the only way to make
// sure other queued handlers don't get executed.
// TODO: this patch must be removed/improved when we rework our actions/operations system
func WrapHandler(handler http.Handler, span ddtrace.Span, pathParams map[string]string, onBlock ...func()) http.Handler {
	instrumentation.SetAppSecEnabledTags(span)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ipTags, clientIP := ClientIPTags(r.Header, true, r.RemoteAddr)
		log.Debug("appsec: http client ip detection returned `%s` given the http headers `%v`", clientIP, r.Header)
		instrumentation.SetStringTags(span, ipTags)

		var bypassHandler http.Handler
		var blocking bool
		args := MakeHandlerOperationArgs(r, clientIP, pathParams)
		ctx, op := StartOperation(r.Context(), args, dyngo.NewDataListener(func(a *sharedsec.Action) {
			bypassHandler = a.HTTP()
			blocking = a.Blocking()
		}))
		r = r.WithContext(ctx)

		defer func() {
			var status int
			if mw, ok := w.(interface{ Status() int }); ok {
				status = mw.Status()
			}

			events := op.Finish(HandlerOperationRes{Status: status})

			// Execute the onBlock functions to make sure blocking works properly
			// in case we are instrumenting the Gin framework
			if blocking {
				op.AddTag(instrumentation.BlockedRequestTag, true)
				for _, f := range onBlock {
					f()
				}
			}

			if bypassHandler != nil {
				bypassHandler.ServeHTTP(w, r)
			}

			// Add the request headers span tags out of args.Headers instead of r.Header as it was normalized and some
			// extra headers have been added such as the Host header which is removed from the original Go request headers
			// map
			setRequestHeadersTags(span, args.Headers)
			setResponseHeadersTags(span, w.Header())
			instrumentation.SetTags(span, op.Tags())
			if len(events) > 0 {
				SetSecurityEventsTags(span, events)
			}
		}()

		if bypassHandler != nil {
			handler = bypassHandler
			bypassHandler = nil
		}
		handler.ServeHTTP(w, r)
	})
}

// MakeHandlerOperationArgs creates the HandlerOperationArgs value.
func MakeHandlerOperationArgs(r *http.Request, clientIP netip.Addr, pathParams map[string]string) HandlerOperationArgs {
	headers := make(http.Header, len(r.Header))
	for k, v := range r.Header {
		k := strings.ToLower(k)
		if k == "cookie" {
			// Do not include cookies in the request headers
			continue
		}
		headers[k] = v
	}
	cookies := makeCookies(r) // TODO(Julio-Guerra): avoid actively parsing the cookies thanks to dynamic instrumentation
	headers["host"] = []string{r.Host}
	return HandlerOperationArgs{
		Method:     r.Method,
		RequestURI: r.RequestURI,
		Headers:    headers,
		Cookies:    cookies,
		Query:      r.URL.Query(), // TODO(Julio-Guerra): avoid actively parsing the query values thanks to dynamic instrumentation
		PathParams: pathParams,
		ClientIP:   clientIP,
	}
}

// MakeHandlerOperationRes creates the HandlerOperationRes value.
func MakeHandlerOperationRes(w http.ResponseWriter) HandlerOperationRes {
	var status int
	if mw, ok := w.(interface{ Status() int }); ok {
		status = mw.Status()
	}
	return HandlerOperationRes{Status: status}
}

// Return the map of parsed cookies if any and following the specification of
// the rule address `server.request.cookies`.
func makeCookies(r *http.Request) map[string][]string {
	parsed := r.Cookies()
	if len(parsed) == 0 {
		return nil
	}
	cookies := make(map[string][]string, len(parsed))
	for _, c := range parsed {
		cookies[c.Name] = append(cookies[c.Name], c.Value)
	}
	return cookies
}

// TODO(Julio-Guerra): create a go-generate tool to generate the types, vars and methods below

// Operation type representing an HTTP operation. It must be created with
// StartOperation() and finished with its Finish().
type (
	Operation struct {
		dyngo.Operation
		instrumentation.TagsHolder
		instrumentation.SecurityEventsHolder
		mu sync.RWMutex
	}

	// SDKBodyOperation type representing an SDK body
	SDKBodyOperation struct {
		dyngo.Operation
	}
)

// StartOperation starts an HTTP handler operation, along with the given
// context and arguments and emits a start event up in the operation stack.
// The operation is linked to the global root operation since an HTTP operation
// is always expected to be first in the operation stack.
func StartOperation(ctx context.Context, args HandlerOperationArgs, listeners ...dyngo.DataListener) (context.Context, *Operation) {
	op := &Operation{
		Operation:  dyngo.NewOperation(nil),
		TagsHolder: instrumentation.NewTagsHolder(),
	}
	for _, l := range listeners {
		op.OnData(l)
	}
	newCtx := context.WithValue(ctx, instrumentation.ContextKey{}, op)
	dyngo.StartOperation(op, args)
	return newCtx, op
}

// fromContext returns the Operation object stored in the context, if any
func fromContext(ctx context.Context) *Operation {
	// Avoid a runtime panic in case of type-assertion error by collecting the 2 return values
	op, _ := ctx.Value(instrumentation.ContextKey{}).(*Operation)
	return op
}

// Finish the HTTP handler operation, along with the given results and emits a
// finish event up in the operation stack.
func (op *Operation) Finish(res HandlerOperationRes) []any {
	dyngo.FinishOperation(op, res)
	return op.Events()
}

// Finish finishes the SDKBody operation and emits a finish event
func (op *SDKBodyOperation) Finish() {
	dyngo.FinishOperation(op, SDKBodyOperationRes{})
}

// HTTP handler operation's start and finish event callback function types.
type (
	// OnHandlerOperationStart function type, called when an HTTP handler
	// operation starts.
	OnHandlerOperationStart func(*Operation, HandlerOperationArgs)
	// OnHandlerOperationFinish function type, called when an HTTP handler
	// operation finishes.
	OnHandlerOperationFinish func(*Operation, HandlerOperationRes)
	// OnSDKBodyOperationStart function type, called when an SDK body
	// operation starts.
	OnSDKBodyOperationStart func(*SDKBodyOperation, SDKBodyOperationArgs)
	// OnSDKBodyOperationFinish function type, called when an SDK body
	// operation finishes.
	OnSDKBodyOperationFinish func(*SDKBodyOperation, SDKBodyOperationRes)
)

var (
	handlerOperationArgsType = reflect.TypeOf((*HandlerOperationArgs)(nil)).Elem()
	handlerOperationResType  = reflect.TypeOf((*HandlerOperationRes)(nil)).Elem()
	sdkBodyOperationArgsType = reflect.TypeOf((*SDKBodyOperationArgs)(nil)).Elem()
	sdkBodyOperationResType  = reflect.TypeOf((*SDKBodyOperationRes)(nil)).Elem()
)

// ListenedType returns the type a OnHandlerOperationStart event listener
// listens to, which is the HandlerOperationArgs type.
func (OnHandlerOperationStart) ListenedType() reflect.Type { return handlerOperationArgsType }

// Call calls the underlying event listener function by performing the
// type-assertion on v whose type is the one returned by ListenedType().
func (f OnHandlerOperationStart) Call(op dyngo.Operation, v interface{}) {
	f(op.(*Operation), v.(HandlerOperationArgs))
}

// ListenedType returns the type a OnHandlerOperationFinish event listener
// listens to, which is the HandlerOperationRes type.
func (OnHandlerOperationFinish) ListenedType() reflect.Type { return handlerOperationResType }

// Call calls the underlying event listener function by performing the
// type-assertion on v whose type is the one returned by ListenedType().
func (f OnHandlerOperationFinish) Call(op dyngo.Operation, v interface{}) {
	f(op.(*Operation), v.(HandlerOperationRes))
}

// ListenedType returns the type a OnSDKBodyOperationStart event listener
// listens to, which is the SDKBodyOperationStartArgs type.
func (OnSDKBodyOperationStart) ListenedType() reflect.Type { return sdkBodyOperationArgsType }

// Call calls the underlying event listener function by performing the
// type-assertion  on v whose type is the one returned by ListenedType().
func (f OnSDKBodyOperationStart) Call(op dyngo.Operation, v interface{}) {
	f(op.(*SDKBodyOperation), v.(SDKBodyOperationArgs))
}

// ListenedType returns the type a OnSDKBodyOperationFinish event listener
// listens to, which is the SDKBodyOperationRes type.
func (OnSDKBodyOperationFinish) ListenedType() reflect.Type { return sdkBodyOperationResType }

// Call calls the underlying event listener function by performing the
// type-assertion on v whose type is the one returned by ListenedType().
func (f OnSDKBodyOperationFinish) Call(op dyngo.Operation, v interface{}) {
	f(op.(*SDKBodyOperation), v.(SDKBodyOperationRes))
}
