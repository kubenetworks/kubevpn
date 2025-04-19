// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"github.com/tinylib/msgp/msgp"
)

var (
	_ msgp.Encodable = (*metaStructMap)(nil)
	_ msgp.Decodable = (*metaStructMap)(nil)
	_ msgp.Sizer     = (*metaStructMap)(nil)
)

// metaStructMap is a map of string to any of metadata embedded in each span
// We export special messagepack methods to handle the encoding and decoding of the map
// Because the agent expects the metadata to be a map of string to byte array, we have to create sub-messages of messagepack for each value
type metaStructMap map[string]any

// EncodeMsg transforms the map[string]any into a map[string][]byte agent-side (which is parsed back into a map[string]any in the backend)
func (m *metaStructMap) EncodeMsg(en *msgp.Writer) error {
	err := en.WriteMapHeader(uint32(len(*m)))
	if err != nil {
		return msgp.WrapError(err, "MetaStruct")
	}

	for key, value := range *m {
		err = en.WriteString(key)
		if err != nil {
			return msgp.WrapError(err, "MetaStruct")
		}

		// Wrap the encoded value in a byte array that will not be parsed by the agent
		msg, err := msgp.AppendIntf(nil, value)
		if err != nil {
			return msgp.WrapError(err, "MetaStruct", key)
		}

		err = en.WriteBytes(msg)
		if err != nil {
			return msgp.WrapError(err, "MetaStruct", key)
		}
	}

	return nil
}

// DecodeMsg transforms the map[string][]byte agent-side into a map[string]any where values are sub-messages in messagepack
func (m *metaStructMap) DecodeMsg(de *msgp.Reader) error {
	header, err := de.ReadMapHeader()
	if err != nil {
		return msgp.WrapError(err, "MetaStruct")
	}

	*m = make(metaStructMap, header)
	for i := uint32(0); i < header; i++ {
		var key string
		key, err = de.ReadString()
		if err != nil {
			return msgp.WrapError(err, "MetaStruct")
		}

		subMsg, err := de.ReadBytes(nil)
		if err != nil {
			return msgp.WrapError(err, "MetaStruct", key)
		}

		(*m)[key], _, err = msgp.ReadIntfBytes(subMsg)
		if err != nil {
			return msgp.WrapError(err, "MetaStruct", key)
		}
	}

	return nil
}

// Msgsize returns an upper bound estimate of the number of bytes occupied by the serialized message
func (m *metaStructMap) Msgsize() int {
	size := msgp.MapHeaderSize
	for key, value := range *m {
		size += msgp.StringPrefixSize + len(key)
		size += msgp.BytesPrefixSize + msgp.GuessSize(value)
	}
	return size
}
