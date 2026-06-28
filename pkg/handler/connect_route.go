package handler

import (
	"time"

	"k8s.io/client-go/tools/cache"
)

// routeDebounceInterval is the delay the route-table ticker is reset to after an
// informer event, debouncing rapid events into a single route-table update.
const routeDebounceInterval = 3 * time.Second

// newTickerResetHandler returns event handler funcs that reset the given ticker
// to routeDebounceInterval on any add, update, or delete event. This debounces
// rapid informer events into a single route-table update.
func newTickerResetHandler(ticker *time.Ticker) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			ticker.Reset(routeDebounceInterval)
		},
		UpdateFunc: func(oldObj, newObj any) {
			ticker.Reset(routeDebounceInterval)
		},
		DeleteFunc: func(obj any) {
			ticker.Reset(routeDebounceInterval)
		},
	}
}
