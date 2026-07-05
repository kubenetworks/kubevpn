package netutil

import (
	"context"
	"runtime/debug"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// HandleCrash recovers from a panic, logs the stack trace, then re-panics.
func HandleCrash() {
	if r := recover(); r != nil {
		plog.GetLogger(context.Background()).Error(r)
		plog.GetLogger(context.Background()).Panicf("Panic: %s", string(debug.Stack()))
		panic(r)
	}
}
