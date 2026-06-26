package handler

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestTcpProbes(t *testing.T) {
	liveness, readiness, startup := tcpProbes(8080)

	if liveness == nil || readiness == nil || startup == nil {
		t.Fatal("expected non-nil probes")
	}
	if liveness.TCPSocket == nil {
		t.Fatal("expected TCP socket handler on liveness probe")
	}
	if liveness.TCPSocket.Port != intstr.FromInt32(8080) {
		t.Errorf("expected port 8080, got %v", liveness.TCPSocket.Port)
	}
	if liveness.InitialDelaySeconds != 5 {
		t.Errorf("liveness InitialDelaySeconds: expected 5, got %d", liveness.InitialDelaySeconds)
	}
	if readiness.InitialDelaySeconds != 3 {
		t.Errorf("readiness InitialDelaySeconds: expected 3, got %d", readiness.InitialDelaySeconds)
	}
	if startup.InitialDelaySeconds != 1 {
		t.Errorf("startup InitialDelaySeconds: expected 1, got %d", startup.InitialDelaySeconds)
	}
}

func TestHttpProbes(t *testing.T) {
	liveness, readiness, startup := httpProbes(80, "/healthz")

	if liveness == nil || readiness == nil || startup == nil {
		t.Fatal("expected non-nil probes")
	}
	if liveness.HTTPGet == nil {
		t.Fatal("expected HTTPGet handler on liveness probe")
	}
	if liveness.HTTPGet.Port != intstr.FromInt32(80) {
		t.Errorf("expected port 80, got %v", liveness.HTTPGet.Port)
	}
	if liveness.HTTPGet.Path != "/healthz" {
		t.Errorf("expected path /healthz, got %s", liveness.HTTPGet.Path)
	}
	if liveness.HTTPGet.Scheme != v1.URISchemeHTTPS {
		t.Errorf("expected HTTPS scheme, got %s", liveness.HTTPGet.Scheme)
	}
}

func TestGenDeploySpec_ImagePullSecret(t *testing.T) {
	deploy := genDeploySpec("ns", "tcp", "envoy", "dns", "http", "img:latest", "my-secret")
	secrets := deploy.Spec.Template.Spec.ImagePullSecrets
	if len(secrets) != 1 || secrets[0].Name != "my-secret" {
		t.Errorf("expected imagePullSecret 'my-secret', got %v", secrets)
	}
}

func TestGenDeploySpec_NoImagePullSecret(t *testing.T) {
	deploy := genDeploySpec("ns", "tcp", "envoy", "dns", "http", "img:latest", "")
	if len(deploy.Spec.Template.Spec.ImagePullSecrets) != 0 {
		t.Errorf("expected no imagePullSecrets, got %v", deploy.Spec.Template.Spec.ImagePullSecrets)
	}
}

func TestGenDeploySpec_ProbesSet(t *testing.T) {
	deploy := genDeploySpec("ns", "tcp", "envoy", "dns", "http", "img:latest", "")
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(containers))
	}
	for i, c := range containers {
		if c.LivenessProbe == nil {
			t.Errorf("container %d (%s): missing liveness probe", i, c.Name)
		}
		if c.ReadinessProbe == nil {
			t.Errorf("container %d (%s): missing readiness probe", i, c.Name)
		}
		if c.StartupProbe == nil {
			t.Errorf("container %d (%s): missing startup probe", i, c.Name)
		}
	}
}
