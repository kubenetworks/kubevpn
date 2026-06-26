package inject

import (
	"testing"

	v1 "k8s.io/api/core/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestAlreadyInjected_VPNOnly(t *testing.T) {
	templateSpec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Name: "app"},
				{Name: config.ContainerSidecarVPN},
			},
		},
	}
	if alreadyInjected(templateSpec) {
		t.Error("expected alreadyInjected to return false when only VPN sidecar is present (no envoy)")
	}
}

func TestAlreadyInjected_EnvoyOnly(t *testing.T) {
	templateSpec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Name: "app"},
				{Name: config.ContainerSidecarEnvoyProxy},
			},
		},
	}
	if alreadyInjected(templateSpec) {
		t.Error("expected alreadyInjected to return false when only envoy sidecar is present (no VPN)")
	}
}
