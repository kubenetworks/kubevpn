// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package datastreams

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strings"
	"time"
)

var hashableEdgeTags = map[string]struct{}{"event_type": {}, "exchange": {}, "group": {}, "topic": {}, "type": {}, "direction": {}}

func isWellFormedEdgeTag(t string) bool {
	if i := strings.IndexByte(t, ':'); i != -1 {
		if j := strings.LastIndexByte(t, ':'); j == i {
			if _, exists := hashableEdgeTags[t[:i]]; exists {
				return true
			}
		}
	}
	return false
}

func nodeHash(service, env string, edgeTags []string) uint64 {
	h := fnv.New64()
	sort.Strings(edgeTags)
	h.Write([]byte(service))
	h.Write([]byte(env))
	for _, t := range edgeTags {
		if isWellFormedEdgeTag(t) {
			h.Write([]byte(t))
		} else {
			fmt.Println("not formatted correctly", t)
		}
	}
	return h.Sum64()
}

func pathwayHash(nodeHash, parentHash uint64) uint64 {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b, nodeHash)
	binary.LittleEndian.PutUint64(b[8:], parentHash)
	h := fnv.New64()
	h.Write(b)
	return h.Sum64()
}

// Pathway is used to monitor how payloads are sent across different services.
// An example Pathway would be:
// service A -- edge 1 --> service B -- edge 2 --> service C
// So it's a branch of services (we also call them "nodes") connected via edges.
// As the payload is sent around, we save the start time (start of service A),
// and the start time of the previous service.
// This allows us to measure the latency of each edge, as well as the latency from origin of any service.
type Pathway struct {
	// hash is the hash of the current node, of the parent node, and of the edge that connects the parent node
	// to this node.
	hash uint64
	// pathwayStart is the start of the first node in the Pathway
	pathwayStart time.Time
	// edgeStart is the start of the previous node.
	edgeStart time.Time
}

// Merge merges multiple pathways into one.
// The current implementation samples one resulting Pathway. A future implementation could be more clever
// and actually merge the Pathways.
func Merge(pathways []Pathway) Pathway {
	if len(pathways) == 0 {
		return Pathway{}
	}
	// Randomly select a pathway to propagate downstream.
	n := rand.Intn(len(pathways))
	return pathways[n]
}

// GetHash gets the hash of a pathway.
func (p Pathway) GetHash() uint64 {
	return p.hash
}

// PathwayStart returns the start timestamp of the pathway
func (p Pathway) PathwayStart() time.Time {
	return p.pathwayStart
}

func (p Pathway) EdgeStart() time.Time {
	return p.edgeStart
}
