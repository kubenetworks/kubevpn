// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package datastreams

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"

	"github.com/DataDog/sketches-go/ddsketch/encoding"
)

type contextKey struct{}

var activePathwayKey = contextKey{}

const (
	// PropagationKeyBase64 is the key to use to propagate the pathway between services.
	PropagationKeyBase64 = "dd-pathway-ctx-base64"
)

// Encode encodes the pathway
func (p Pathway) Encode() []byte {
	data := make([]byte, 8, 20)
	binary.LittleEndian.PutUint64(data, p.hash)
	encoding.EncodeVarint64(&data, p.pathwayStart.UnixNano()/int64(time.Millisecond))
	encoding.EncodeVarint64(&data, p.edgeStart.UnixNano()/int64(time.Millisecond))
	return data
}

// Decode decodes a pathway
func Decode(ctx context.Context, data []byte) (p Pathway, outCtx context.Context, err error) {
	if len(data) < 8 {
		return p, ctx, errors.New("hash smaller than 8 bytes")
	}
	p.hash = binary.LittleEndian.Uint64(data)
	data = data[8:]
	pathwayStart, err := encoding.DecodeVarint64(&data)
	if err != nil {
		return p, ctx, err
	}
	edgeStart, err := encoding.DecodeVarint64(&data)
	if err != nil {
		return p, ctx, err
	}
	p.pathwayStart = time.Unix(0, pathwayStart*int64(time.Millisecond))
	p.edgeStart = time.Unix(0, edgeStart*int64(time.Millisecond))
	return p, ContextWithPathway(ctx, p), nil
}

// EncodeBase64 encodes a pathway context into a string using base64 encoding.
func (p Pathway) EncodeBase64() string {
	b := p.Encode()
	return base64.StdEncoding.EncodeToString(b)
}

// DecodeBase64 decodes a pathway context from a string using base64 encoding.
func DecodeBase64(ctx context.Context, str string) (p Pathway, outCtx context.Context, err error) {
	data, err := base64.StdEncoding.DecodeString(str)
	if err != nil {
		return p, ctx, err
	}
	return Decode(ctx, data)
}

// ContextWithPathway returns a copy of the given context which includes the pathway p.
func ContextWithPathway(ctx context.Context, p Pathway) context.Context {
	return context.WithValue(ctx, activePathwayKey, p)
}

// PathwayFromContext returns the pathway contained in the given context, and whether a
// pathway is found in ctx.
func PathwayFromContext(ctx context.Context) (p Pathway, ok bool) {
	if ctx == nil {
		return p, false
	}
	v := ctx.Value(activePathwayKey)
	if p, ok := v.(Pathway); ok {
		return p, true
	}
	return p, false
}
