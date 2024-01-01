// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package internal

import (
	"sync"
	"sync/atomic"
)

// LockMap uses an RWMutex to synchronize map access to allow for concurrent access.
// This should not be used for cases with heavy write load and performance concerns.
type LockMap struct {
	sync.RWMutex
	c uint32
	m map[string]string
}

func NewLockMap(m map[string]string) *LockMap {
	return &LockMap{m: m, c: uint32(len(m))}
}

// Iter iterates over all the map entries passing in keys and values to provided func f. Note this is READ ONLY.
func (l *LockMap) Iter(f func(key string, val string)) {
	c := atomic.LoadUint32(&l.c)
	if c == 0 { //Fast exit to avoid the cost of RLock/RUnlock for empty maps
		return
	}
	l.RLock()
	defer l.RUnlock()
	for k, v := range l.m {
		f(k, v)
	}
}

func (l *LockMap) Len() int {
	l.RLock()
	defer l.RUnlock()
	return len(l.m)
}

func (l *LockMap) Clear() {
	l.Lock()
	defer l.Unlock()
	l.m = map[string]string{}
	atomic.StoreUint32(&l.c, 0)
}

func (l *LockMap) Set(k, v string) {
	l.Lock()
	defer l.Unlock()
	if _, ok := l.m[k]; !ok {
		atomic.AddUint32(&l.c, 1)
	}
	l.m[k] = v
}

func (l *LockMap) Get(k string) string {
	l.RLock()
	defer l.RUnlock()
	return l.m[k]
}
