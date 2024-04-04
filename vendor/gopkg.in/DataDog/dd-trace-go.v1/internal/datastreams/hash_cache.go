// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package datastreams

import (
	"strings"
	"sync"
)

const (
	maxHashCacheSize = 1000
)

type hashCache struct {
	mu sync.RWMutex
	m  map[string]uint64
}

func getHashKey(edgeTags []string, parentHash uint64) string {
	var s strings.Builder
	l := 0
	for _, t := range edgeTags {
		l += len(t)
	}
	l += 8
	s.Grow(l)
	for _, t := range edgeTags {
		s.WriteString(t)
	}
	s.WriteByte(byte(parentHash))
	s.WriteByte(byte(parentHash >> 8))
	s.WriteByte(byte(parentHash >> 16))
	s.WriteByte(byte(parentHash >> 24))
	s.WriteByte(byte(parentHash >> 32))
	s.WriteByte(byte(parentHash >> 40))
	s.WriteByte(byte(parentHash >> 48))
	s.WriteByte(byte(parentHash >> 56))
	return s.String()
}

func (c *hashCache) computeAndGet(key string, parentHash uint64, service, env string, edgeTags []string) uint64 {
	hash := pathwayHash(nodeHash(service, env, edgeTags), parentHash)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= maxHashCacheSize {
		// high cardinality of hashes shouldn't happen in practice, due to a limited amount of topics consumed
		// by each service.
		c.m = make(map[string]uint64)
	}
	c.m[key] = hash
	return hash
}

func (c *hashCache) get(service, env string, edgeTags []string, parentHash uint64) uint64 {
	key := getHashKey(edgeTags, parentHash)
	c.mu.RLock()
	if hash, ok := c.m[key]; ok {
		c.mu.RUnlock()
		return hash
	}
	c.mu.RUnlock()
	return c.computeAndGet(key, parentHash, service, env, edgeTags)
}

func newHashCache() *hashCache {
	return &hashCache{m: make(map[string]uint64)}
}
