// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

//go:generate msgp -o=stacktrace_msgp.go -tests=false

package stacktrace

import (
	"errors"
	"os"
	"regexp"
	"runtime"
	"strings"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"

	"github.com/eapache/queue/v2"
	"github.com/hashicorp/go-secure-stdlib/parseutil"
)

var (
	enabled              = true
	defaultTopFrameDepth = 8
	defaultMaxDepth      = 32

	// internalPackagesPrefixes is the list of prefixes for internal packages that should be hidden in the stack trace
	internalSymbolPrefixes = []string{
		"gopkg.in/DataDog/dd-trace-go.v1",
		"github.com/DataDog/dd-trace-go",
		"github.com/DataDog/go-libddwaf",
		"github.com/DataDog/datadog-agent",
		"github.com/DataDog/appsec-internal-go",
		"github.com/datadog/orchestrion",
		"github.com/DataDog/orchestrion",
	}
)

const (
	defaultCallerSkip    = 4
	envStackTraceDepth   = "DD_APPSEC_MAX_STACK_TRACE_DEPTH"
	envStackTraceEnabled = "DD_APPSEC_STACK_TRACE_ENABLE"
)

func init() {
	if env := os.Getenv(envStackTraceEnabled); env != "" {
		if e, err := parseutil.ParseBool(env); err == nil {
			enabled = e
		} else {
			log.Error("Failed to parse %s env var as boolean: %v (using default value: %v)", envStackTraceEnabled, err, enabled)
		}
	}

	if env := os.Getenv(envStackTraceDepth); env != "" {
		if !enabled {
			log.Warn("Ignoring %s because stacktrace generation is disable", envStackTraceDepth)
			return
		}

		if depth, err := parseutil.SafeParseInt(env); err == nil {
			defaultMaxDepth = depth
		} else {
			if depth <= 0 && err == nil {
				err = errors.New("value is not a strictly positive integer")
			}
			log.Error("Failed to parse %s env var as a positive integer: %v (using default value: %v)", envStackTraceDepth, err, defaultMaxDepth)
		}
	}

	defaultTopFrameDepth = defaultMaxDepth / 4
}

// Enabled returns whether stacktrace should be collected
func Enabled() bool {
	return enabled
}

// StackTrace is intended to be sent over the span tag `_dd.stack`, the first frame is the current frame
type StackTrace []StackFrame

// StackFrame represents a single frame in the stack trace
type StackFrame struct {
	Index     uint32 `msg:"id"`                   // Index of the frame (0 = top of the stack)
	Text      string `msg:"text,omitempty"`       // Text version of the stackframe as a string
	File      string `msg:"file,omitempty"`       // File name where the code line is
	Line      uint32 `msg:"line,omitempty"`       // Line number in the context of the file where the code is
	Column    uint32 `msg:"column,omitempty"`     // Column where the code ran is
	Namespace string `msg:"namespace,omitempty"`  // Namespace is the fully qualified name of the package where the code is
	ClassName string `msg:"class_name,omitempty"` // ClassName is the fully qualified name of the class where the line of code is
	Function  string `msg:"function,omitempty"`   // Function is the fully qualified name of the function where the line of code is
}

type symbol struct {
	Package  string
	Receiver string
	Function string
}

var symbolRegex = regexp.MustCompile(`^(([^(]+/)?([^(/.]+)?)(\.\(([^/)]+)\))?\.([^/()]+)$`)

// parseSymbol parses a symbol name into its package, receiver and function
// ex: gopkg.in/DataDog/dd-trace-go.v1/internal/stacktrace.(*Event).NewException
// -> package: gopkg.in/DataDog/dd-trace-go.v1/internal/stacktrace
// -> receiver: *Event
// -> function: NewException
func parseSymbol(name string) symbol {
	matches := symbolRegex.FindStringSubmatch(name)
	if len(matches) != 7 {
		log.Error("Failed to parse symbol for stacktrace: %s", name)
		return symbol{
			Package:  "",
			Receiver: "",
			Function: "",
		}
	}

	return symbol{
		Package:  matches[1],
		Receiver: matches[5],
		Function: matches[6],
	}
}

// Capture create a new stack trace from the current call stack
func Capture() StackTrace {
	return SkipAndCapture(defaultCallerSkip)
}

// SkipAndCapture creates a new stack trace from the current call stack, skipping the first `skip` frames
func SkipAndCapture(skip int) StackTrace {
	return skipAndCapture(skip, defaultMaxDepth, internalSymbolPrefixes)
}

func skipAndCapture(skip int, maxDepth int, symbolSkip []string) StackTrace {
	iter := iterator(skip, maxDepth, symbolSkip)
	stack := make([]StackFrame, defaultMaxDepth)
	nbStoredFrames := 0
	topFramesQueue := queue.New[StackFrame]()

	// We have to make sure we don't store more than maxDepth frames
	// if there is more than maxDepth frames, we get X frames from the bottom of the stack and Y from the top
	for frame, ok := iter.Next(); ok; frame, ok = iter.Next() {
		// we reach the top frames: start to use the queue
		if nbStoredFrames >= defaultMaxDepth-defaultTopFrameDepth {
			topFramesQueue.Add(frame)
			// queue is full, remove the oldest frame
			if topFramesQueue.Length() > defaultTopFrameDepth {
				topFramesQueue.Remove()
			}
			continue
		}

		// Bottom frames: directly store them in the stack
		stack[nbStoredFrames] = frame
		nbStoredFrames++
	}

	// Stitch the top frames to the stack
	for topFramesQueue.Length() > 0 {
		stack[nbStoredFrames] = topFramesQueue.Remove()
		nbStoredFrames++
	}

	return stack[:nbStoredFrames]
}

// framesIterator is an iterator over the frames of a call stack
// It skips internal packages and caches the frames to avoid multiple calls to runtime.Callers
// It also skips the first `skip` frames
// It is not thread-safe
type framesIterator struct {
	skipPrefixes []string
	cache        []uintptr
	frames       *queue.Queue[runtime.Frame]
	cacheDepth   int
	cacheSize    int
	currDepth    int
}

func iterator(skip, cacheSize int, internalPrefixSkip []string) framesIterator {
	return framesIterator{
		skipPrefixes: internalPrefixSkip,
		cache:        make([]uintptr, cacheSize),
		frames:       queue.New[runtime.Frame](),
		cacheDepth:   skip,
		cacheSize:    cacheSize,
		currDepth:    0,
	}
}

// next returns the next runtime.Frame in the call stack, filling the cache if needed
func (it *framesIterator) next() (runtime.Frame, bool) {
	if it.frames.Length() == 0 {
		n := runtime.Callers(it.cacheDepth, it.cache)
		if n == 0 {
			return runtime.Frame{}, false
		}

		frames := runtime.CallersFrames(it.cache[:n])
		for {
			frame, more := frames.Next()
			it.frames.Add(frame)
			it.cacheDepth++
			if !more {
				break
			}
		}
	}

	it.currDepth++
	return it.frames.Remove(), true
}

// Next returns the next StackFrame in the call stack, skipping internal packages and refurbishing the cache if needed
func (it *framesIterator) Next() (StackFrame, bool) {
	for {
		frame, ok := it.next()
		if !ok {
			return StackFrame{}, false
		}

		if it.skipFrame(frame) {
			continue
		}

		parsedSymbol := parseSymbol(frame.Function)
		return StackFrame{
			Index:     uint32(it.currDepth - 1),
			Text:      "",
			File:      frame.File,
			Line:      uint32(frame.Line),
			Column:    0, // No column given by the runtime
			Namespace: parsedSymbol.Package,
			ClassName: parsedSymbol.Receiver,
			Function:  parsedSymbol.Function,
		}, true
	}
}

func (it *framesIterator) skipFrame(frame runtime.Frame) bool {
	if frame.File == "<generated>" { // skip orchestrion generated code
		return true
	}

	for _, prefix := range it.skipPrefixes {
		if strings.HasPrefix(frame.Function, prefix) {
			return true
		}
	}

	return false
}
