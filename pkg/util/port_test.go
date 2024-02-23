package util

import (
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
)

func TestSplitPort(t *testing.T) {
	testDataList := []struct {
		str    string
		expect v1.ContainerPort
	}{
		{
			str: "tcp/80:8080",
			expect: v1.ContainerPort{
				HostPort:      8080,
				ContainerPort: 80,
				Protocol:      v1.ProtocolTCP,
			},
		},
		{
			str: "tcp/80:8080",
			expect: v1.ContainerPort{
				HostPort:      8080,
				ContainerPort: 80,
				Protocol:      v1.ProtocolTCP,
			},
		},
		{
			str: "udp/5000:50512",
			expect: v1.ContainerPort{
				HostPort:      50512,
				ContainerPort: 5000,
				Protocol:      v1.ProtocolUDP,
			},
		},
		{
			str: "UDP/5000:50512",
			expect: v1.ContainerPort{
				HostPort:      50512,
				ContainerPort: 5000,
				Protocol:      v1.ProtocolUDP,
			},
		},
		{
			str: "80:8080",
			expect: v1.ContainerPort{
				HostPort:      8080,
				ContainerPort: 80,
				Protocol:      v1.ProtocolTCP,
			},
		},
		{
			str: "tcp/80",
			expect: v1.ContainerPort{
				HostPort:      80,
				ContainerPort: 80,
				Protocol:      v1.ProtocolTCP,
			},
		},
		{
			str: "udp/5000",
			expect: v1.ContainerPort{
				HostPort:      5000,
				ContainerPort: 5000,
				Protocol:      v1.ProtocolUDP,
			},
		},
	}
	for _, d := range testDataList {
		port := ParsePort(d.str)
		if !reflect.DeepEqual(port, d.expect) {
			t.Errorf("parse str: %s, expected: %v, real: %v", d.str, d, port)
		}
	}
}
