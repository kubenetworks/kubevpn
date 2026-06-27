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

	// All three must have TCPSocket handler on the same port
	for name, probe := range map[string]*v1.Probe{"liveness": liveness, "readiness": readiness, "startup": startup} {
		if probe.TCPSocket == nil {
			t.Fatalf("%s: expected TCP socket handler", name)
		}
		if probe.TCPSocket.Port != intstr.FromInt32(8080) {
			t.Errorf("%s: expected port 8080, got %v", name, probe.TCPSocket.Port)
		}
	}

	// Timing: liveness is most lenient, startup is most aggressive
	if liveness.InitialDelaySeconds != 5 || liveness.PeriodSeconds != 15 || liveness.FailureThreshold != 3 {
		t.Errorf("liveness timing: delay=%d period=%d failures=%d", liveness.InitialDelaySeconds, liveness.PeriodSeconds, liveness.FailureThreshold)
	}
	if readiness.InitialDelaySeconds != 3 || readiness.PeriodSeconds != 10 || readiness.FailureThreshold != 3 {
		t.Errorf("readiness timing: delay=%d period=%d failures=%d", readiness.InitialDelaySeconds, readiness.PeriodSeconds, readiness.FailureThreshold)
	}
	if startup.InitialDelaySeconds != 1 || startup.PeriodSeconds != 2 || startup.FailureThreshold != 15 {
		t.Errorf("startup timing: delay=%d period=%d failures=%d", startup.InitialDelaySeconds, startup.PeriodSeconds, startup.FailureThreshold)
	}
}

func TestGenDeploySpec_ImagePullSecret(t *testing.T) {
	deploy := genDeploySpec("ns", "img:latest", "my-secret")
	secrets := deploy.Spec.Template.Spec.ImagePullSecrets
	if len(secrets) != 1 || secrets[0].Name != "my-secret" {
		t.Errorf("expected imagePullSecret 'my-secret', got %v", secrets)
	}
}

func TestGenDeploySpec_NoImagePullSecret(t *testing.T) {
	deploy := genDeploySpec("ns", "img:latest", "")
	if len(deploy.Spec.Template.Spec.ImagePullSecrets) != 0 {
		t.Errorf("expected no imagePullSecrets, got %v", deploy.Spec.Template.Spec.ImagePullSecrets)
	}
}

func TestGenDeploySpec_ProbesSet(t *testing.T) {
	deploy := genDeploySpec("ns", "img:latest", "")
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(containers))
	}
	// VPN and control-plane containers have probes; DNS container does not
	for _, c := range containers[:2] {
		if c.LivenessProbe == nil {
			t.Errorf("container %s: missing liveness probe", c.Name)
		}
		if c.ReadinessProbe == nil {
			t.Errorf("container %s: missing readiness probe", c.Name)
		}
		if c.StartupProbe == nil {
			t.Errorf("container %s: missing startup probe", c.Name)
		}
	}
}
