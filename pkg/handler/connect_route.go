package handler

import (
	"time"

	"k8s.io/client-go/tools/cache"
)

// newTickerResetHandler returns event handler funcs that reset the given ticker
// to 3 seconds on any add, update, or delete event. This debounces rapid
// informer events into a single route-table update.
func newTickerResetHandler(ticker *time.Ticker) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			ticker.Reset(time.Second * 3)
		},
		UpdateFunc: func(oldObj, newObj any) {
			ticker.Reset(time.Second * 3)
		},
		DeleteFunc: func(obj any) {
			ticker.Reset(time.Second * 3)
		},
	}
}
