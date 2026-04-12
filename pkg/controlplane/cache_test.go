package controlplane

import (
	"testing"

	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"

	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	httpconnectionmanager "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
)

func extractHCM(t *testing.T, listener *listener.Listener) *httpconnectionmanager.HttpConnectionManager {
	t.Helper()
	for _, fc := range listener.FilterChains {
		for _, f := range fc.Filters {
			if f.Name == wellknown.HTTPConnectionManager {
				anyMsg := f.GetTypedConfig()
				if anyMsg != nil {
					hcm := &httpconnectionmanager.HttpConnectionManager{}
					if err := anyMsg.UnmarshalTo(hcm); err != nil {
						t.Fatalf("failed to unmarshal HCM: %v", err)
					}
					return hcm
				}
			}
		}
	}
	return nil
}

func TestToListenerNoGrpcWeb(t *testing.T) {
	tests := []struct {
		name          string
		listenerName  string
		routeName     string
		port          int32
		protocol      corev1.Protocol
		isFargateMode bool
	}{
		{
			name:          "TCP non-fargate",
			listenerName:  "test_listener",
			routeName:     "test_route",
			port:          8080,
			protocol:      corev1.ProtocolTCP,
			isFargateMode: false,
		},
		{
			name:          "TCP fargate",
			listenerName:  "test_listener_fargate",
			routeName:     "test_route_fargate",
			port:          8081,
			protocol:      corev1.ProtocolTCP,
			isFargateMode: true,
		},
		{
			name:          "UDP non-fargate",
			listenerName:  "test_listener_udp",
			routeName:     "test_route_udp",
			port:          8082,
			protocol:      corev1.ProtocolUDP,
			isFargateMode: false,
		},
		{
			name:          "SCTP non-fargate",
			listenerName:  "test_listener_sctp",
			routeName:     "test_route_sctp",
			port:          8083,
			protocol:      corev1.ProtocolSCTP,
			isFargateMode: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listener := ToListener(tt.listenerName, tt.routeName, tt.port, tt.protocol, tt.isFargateMode)
			hcm := extractHCM(t, listener)
			// HTTP connection manager should always be present (filter chain is always created)
			assert.NotNil(t, hcm, "HTTP connection manager should be present")
			if hcm == nil {
				return
			}
			// Check that grpc-web filter is not present
			for _, hf := range hcm.HttpFilters {
				assert.NotEqual(t, wellknown.GRPCWeb, hf.Name, "HTTP filters should not contain grpc-web filter")
			}
			// Ensure only expected filters are present
			assert.Len(t, hcm.HttpFilters, 2, "Expected exactly two HTTP filters")
			assert.Equal(t, wellknown.CORS, hcm.HttpFilters[0].Name, "First filter should be CORS")
			assert.Equal(t, wellknown.Router, hcm.HttpFilters[1].Name, "Second filter should be Router")
		})
	}
}
