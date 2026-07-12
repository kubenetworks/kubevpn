package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func TestGenServiceAccount(t *testing.T) {
	namespace := "test-ns"
	sa := genServiceAccount(namespace)

	if sa.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected name %q, got %q", config.ConfigMapPodTrafficManager, sa.Name)
	}
	if sa.Namespace != namespace {
		t.Fatalf("expected namespace %q, got %q", namespace, sa.Namespace)
	}
	if sa.AutomountServiceAccountToken == nil || !*sa.AutomountServiceAccountToken {
		t.Fatal("expected AutomountServiceAccountToken to be true")
	}
}

func TestGenRole(t *testing.T) {
	namespace := "test-ns"
	role := genRole(namespace)

	if role.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected name %q, got %q", config.ConfigMapPodTrafficManager, role.Name)
	}
	if role.Namespace != namespace {
		t.Fatalf("expected namespace %q, got %q", namespace, role.Namespace)
	}
	// Rule 0: configmaps/secrets; Rules 1-2: CIDR detection (see genRole / docs/46).
	if len(role.Rules) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(role.Rules))
	}

	rule := role.Rules[0]
	expectedVerbs := []string{"get", "list", "watch", "create", "update", "patch", "delete"}
	if len(rule.Verbs) != len(expectedVerbs) {
		t.Fatalf("expected %d verbs, got %d", len(expectedVerbs), len(rule.Verbs))
	}
	for i, v := range expectedVerbs {
		if rule.Verbs[i] != v {
			t.Fatalf("expected verb %q at index %d, got %q", v, i, rule.Verbs[i])
		}
	}

	if len(rule.APIGroups) != 1 || rule.APIGroups[0] != "" {
		t.Fatalf("expected APIGroups [\"\"], got %v", rule.APIGroups)
	}

	expectedResources := []string{"configmaps", "secrets"}
	if len(rule.Resources) != len(expectedResources) {
		t.Fatalf("expected %d resources, got %d", len(expectedResources), len(rule.Resources))
	}
	for i, r := range expectedResources {
		if rule.Resources[i] != r {
			t.Fatalf("expected resource %q at index %d, got %q", r, i, rule.Resources[i])
		}
	}

	if len(rule.ResourceNames) != 1 || rule.ResourceNames[0] != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected ResourceNames [%q], got %v", config.ConfigMapPodTrafficManager, rule.ResourceNames)
	}
}

func TestGenRoleBinding(t *testing.T) {
	namespace := "test-ns"
	rb := genRoleBinding(namespace)

	if rb.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected name %q, got %q", config.ConfigMapPodTrafficManager, rb.Name)
	}
	if rb.Namespace != namespace {
		t.Fatalf("expected namespace %q, got %q", namespace, rb.Namespace)
	}

	// Subjects
	if len(rb.Subjects) != 1 {
		t.Fatalf("expected 1 subject, got %d", len(rb.Subjects))
	}
	subj := rb.Subjects[0]
	if subj.Kind != "ServiceAccount" {
		t.Fatalf("expected subject kind %q, got %q", "ServiceAccount", subj.Kind)
	}
	if subj.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected subject name %q, got %q", config.ConfigMapPodTrafficManager, subj.Name)
	}
	if subj.Namespace != namespace {
		t.Fatalf("expected subject namespace %q, got %q", namespace, subj.Namespace)
	}

	// RoleRef
	if rb.RoleRef.APIGroup != "rbac.authorization.k8s.io" {
		t.Fatalf("expected roleRef APIGroup %q, got %q", "rbac.authorization.k8s.io", rb.RoleRef.APIGroup)
	}
	if rb.RoleRef.Kind != "Role" {
		t.Fatalf("expected roleRef kind %q, got %q", "Role", rb.RoleRef.Kind)
	}
	if rb.RoleRef.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected roleRef name %q, got %q", config.ConfigMapPodTrafficManager, rb.RoleRef.Name)
	}
}

func TestGenService(t *testing.T) {
	namespace := "test-ns"
	svc := genService(namespace)

	if svc.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected name %q, got %q", config.ConfigMapPodTrafficManager, svc.Name)
	}
	if svc.Namespace != namespace {
		t.Fatalf("expected namespace %q, got %q", namespace, svc.Namespace)
	}
	if svc.Spec.Type != v1.ServiceTypeClusterIP {
		t.Fatalf("expected service type %q, got %q", v1.ServiceTypeClusterIP, svc.Spec.Type)
	}

	// Selector
	if svc.Spec.Selector["app"] != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected selector app=%q, got %q", config.ConfigMapPodTrafficManager, svc.Spec.Selector["app"])
	}

	// 3 ports
	if len(svc.Spec.Ports) != 3 {
		t.Fatalf("expected 3 ports, got %d", len(svc.Spec.Ports))
	}

	type portCase struct {
		name     string
		port     int32
		protocol v1.Protocol
	}
	expected := []portCase{
		{config.PortNameTCP, 10801, v1.ProtocolTCP},
		{config.PortNameEnvoy, 9002, v1.ProtocolTCP},
		{config.PortNameDNS, 53, v1.ProtocolUDP},
	}
	for i, e := range expected {
		p := svc.Spec.Ports[i]
		if p.Name != e.name {
			t.Errorf("port[%d]: expected name %q, got %q", i, e.name, p.Name)
		}
		if p.Port != e.port {
			t.Errorf("port[%d]: expected port %d, got %d", i, e.port, p.Port)
		}
		if p.Protocol != e.protocol {
			t.Errorf("port[%d]: expected protocol %q, got %q", i, e.protocol, p.Protocol)
		}
		if p.TargetPort.IntValue() != int(e.port) {
			t.Errorf("port[%d]: expected targetPort %d, got %d", i, e.port, p.TargetPort.IntValue())
		}
	}
}

func TestGenSecret(t *testing.T) {
	namespace := "test-ns"
	crt := []byte("fake-cert-data")
	key := []byte("fake-key-data")
	host := []byte("example.com")

	secret := genSecret(namespace, crt, key, host)

	if secret.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected name %q, got %q", config.ConfigMapPodTrafficManager, secret.Name)
	}
	if secret.Namespace != namespace {
		t.Fatalf("expected namespace %q, got %q", namespace, secret.Namespace)
	}
	if secret.Type != v1.SecretTypeOpaque {
		t.Fatalf("expected secret type %q, got %q", v1.SecretTypeOpaque, secret.Type)
	}

	// Check TLS data keys
	if string(secret.Data[config.TLSCertKey]) != string(crt) {
		t.Fatalf("expected TLSCertKey data %q, got %q", crt, secret.Data[config.TLSCertKey])
	}
	if string(secret.Data[config.TLSPrivateKeyKey]) != string(key) {
		t.Fatalf("expected TLSPrivateKeyKey data %q, got %q", key, secret.Data[config.TLSPrivateKeyKey])
	}
	if string(secret.Data[config.TLSServerName]) != string(host) {
		t.Fatalf("expected TLSServerName data %q, got %q", host, secret.Data[config.TLSServerName])
	}
}

func TestGenDeploySpec(t *testing.T) {
	namespace := "test-ns"
	image := "docker.io/naison/kubevpn:test"
	imagePullSecret := "my-pull-secret"

	deploy := genDeploySpec(namespace, image, imagePullSecret)

	if deploy.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected name %q, got %q", config.ConfigMapPodTrafficManager, deploy.Name)
	}
	if deploy.Namespace != namespace {
		t.Fatalf("expected namespace %q, got %q", namespace, deploy.Namespace)
	}

	// Replicas
	if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 1 {
		t.Fatal("expected replicas = 1")
	}

	// Selector
	if deploy.Spec.Selector.MatchLabels["app"] != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected selector app=%q, got %v", config.ConfigMapPodTrafficManager, deploy.Spec.Selector.MatchLabels)
	}

	// Template labels
	if deploy.Spec.Template.Labels["app"] != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected template label app=%q, got %v", config.ConfigMapPodTrafficManager, deploy.Spec.Template.Labels)
	}

	// ServiceAccountName
	if deploy.Spec.Template.Spec.ServiceAccountName != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected serviceAccountName %q, got %q", config.ConfigMapPodTrafficManager, deploy.Spec.Template.Spec.ServiceAccountName)
	}

	// 3 containers
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(containers))
	}

	// Container 0: VPN
	vpn := containers[0]
	if vpn.Name != config.ContainerSidecarVPN {
		t.Fatalf("expected container[0] name %q, got %q", config.ContainerSidecarVPN, vpn.Name)
	}
	if vpn.Image != image {
		t.Fatalf("expected container[0] image %q, got %q", image, vpn.Image)
	}
	if len(vpn.Ports) != 1 || vpn.Ports[0].ContainerPort != 10801 {
		t.Fatalf("expected container[0] port 10801, got %v", vpn.Ports)
	}
	if vpn.LivenessProbe == nil || vpn.ReadinessProbe == nil || vpn.StartupProbe == nil {
		t.Fatal("expected container[0] to have all three probes")
	}

	// Container 1: Control Plane
	cp := containers[1]
	if cp.Name != config.ContainerSidecarXDS {
		t.Fatalf("expected container[1] name %q, got %q", config.ContainerSidecarXDS, cp.Name)
	}
	if cp.Image != image {
		t.Fatalf("expected container[1] image %q, got %q", image, cp.Image)
	}
	if len(cp.Ports) != 1 {
		t.Fatalf("expected container[1] to have 1 port, got %d", len(cp.Ports))
	}
	if cp.Ports[0].ContainerPort != 9002 {
		t.Fatalf("expected container[1] port 9002, got %d", cp.Ports[0].ContainerPort)
	}
	if cp.LivenessProbe == nil || cp.ReadinessProbe == nil || cp.StartupProbe == nil {
		t.Fatal("expected container[1] to have all three probes")
	}

	// Container 2: DNS
	dns := containers[2]
	if dns.Name != config.ContainerSidecarDNS {
		t.Fatalf("expected container[2] name %q, got %q", config.ContainerSidecarDNS, dns.Name)
	}
	if dns.Image != image {
		t.Fatalf("expected container[2] image %q, got %q", image, dns.Image)
	}
	if len(dns.Ports) != 1 || dns.Ports[0].ContainerPort != 53 {
		t.Fatalf("expected container[2] port 53, got %v", dns.Ports)
	}

	// ImagePullSecrets
	if len(deploy.Spec.Template.Spec.ImagePullSecrets) != 1 {
		t.Fatalf("expected 1 imagePullSecret, got %d", len(deploy.Spec.Template.Spec.ImagePullSecrets))
	}
	if deploy.Spec.Template.Spec.ImagePullSecrets[0].Name != imagePullSecret {
		t.Fatalf("expected imagePullSecret %q, got %q", imagePullSecret, deploy.Spec.Template.Spec.ImagePullSecrets[0].Name)
	}

	// RestartPolicy
	if deploy.Spec.Template.Spec.RestartPolicy != v1.RestartPolicyAlways {
		t.Fatalf("expected RestartPolicy %q, got %q", v1.RestartPolicyAlways, deploy.Spec.Template.Spec.RestartPolicy)
	}
}

func TestGenDeploySpecNoImagePullSecret(t *testing.T) {
	deploy := genDeploySpec("ns", "img:latest", "")

	if len(deploy.Spec.Template.Spec.ImagePullSecrets) != 0 {
		t.Fatalf("expected no imagePullSecrets when name is empty, got %d", len(deploy.Spec.Template.Spec.ImagePullSecrets))
	}
}

func TestGenDeploySpec_Probes(t *testing.T) {
	deploy := genDeploySpec("test-ns", "img:latest", "")
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(containers))
	}

	// VPN container: TCP probes on port 10802 (PortUDP)
	vpn := containers[0]
	if vpn.LivenessProbe == nil || vpn.LivenessProbe.TCPSocket == nil {
		t.Fatal("VPN container: expected liveness probe with TCPSocket")
	}
	if vpn.LivenessProbe.TCPSocket.Port.IntValue() != 10802 {
		t.Fatalf("VPN container: expected liveness probe port 10802, got %d", vpn.LivenessProbe.TCPSocket.Port.IntValue())
	}
	if vpn.LivenessProbe.InitialDelaySeconds != 5 || vpn.LivenessProbe.PeriodSeconds != 15 || vpn.LivenessProbe.FailureThreshold != 3 {
		t.Fatalf("VPN container: unexpected liveness probe timing: initial=%d period=%d failure=%d",
			vpn.LivenessProbe.InitialDelaySeconds, vpn.LivenessProbe.PeriodSeconds, vpn.LivenessProbe.FailureThreshold)
	}

	if vpn.ReadinessProbe == nil || vpn.ReadinessProbe.TCPSocket == nil {
		t.Fatal("VPN container: expected readiness probe with TCPSocket")
	}
	if vpn.ReadinessProbe.TCPSocket.Port.IntValue() != 10802 {
		t.Fatalf("VPN container: expected readiness probe port 10802, got %d", vpn.ReadinessProbe.TCPSocket.Port.IntValue())
	}
	if vpn.ReadinessProbe.InitialDelaySeconds != 3 || vpn.ReadinessProbe.PeriodSeconds != 10 || vpn.ReadinessProbe.FailureThreshold != 3 {
		t.Fatalf("VPN container: unexpected readiness probe timing: initial=%d period=%d failure=%d",
			vpn.ReadinessProbe.InitialDelaySeconds, vpn.ReadinessProbe.PeriodSeconds, vpn.ReadinessProbe.FailureThreshold)
	}

	if vpn.StartupProbe == nil || vpn.StartupProbe.TCPSocket == nil {
		t.Fatal("VPN container: expected startup probe with TCPSocket")
	}
	if vpn.StartupProbe.TCPSocket.Port.IntValue() != 10802 {
		t.Fatalf("VPN container: expected startup probe port 10802, got %d", vpn.StartupProbe.TCPSocket.Port.IntValue())
	}
	if vpn.StartupProbe.InitialDelaySeconds != 1 || vpn.StartupProbe.PeriodSeconds != 2 || vpn.StartupProbe.FailureThreshold != 15 {
		t.Fatalf("VPN container: unexpected startup probe timing: initial=%d period=%d failure=%d",
			vpn.StartupProbe.InitialDelaySeconds, vpn.StartupProbe.PeriodSeconds, vpn.StartupProbe.FailureThreshold)
	}

	// Control Plane container: TCP probes on port 9002
	cp := containers[1]
	if cp.LivenessProbe == nil || cp.LivenessProbe.TCPSocket == nil {
		t.Fatal("XDS container: expected liveness probe with TCPSocket")
	}
	if cp.LivenessProbe.TCPSocket.Port.IntValue() != 9002 {
		t.Fatalf("XDS container: expected liveness probe port 9002, got %d", cp.LivenessProbe.TCPSocket.Port.IntValue())
	}
	if cp.LivenessProbe.InitialDelaySeconds != 5 || cp.LivenessProbe.PeriodSeconds != 15 || cp.LivenessProbe.FailureThreshold != 3 {
		t.Fatalf("XDS container: unexpected liveness probe timing: initial=%d period=%d failure=%d",
			cp.LivenessProbe.InitialDelaySeconds, cp.LivenessProbe.PeriodSeconds, cp.LivenessProbe.FailureThreshold)
	}

	if cp.ReadinessProbe == nil || cp.ReadinessProbe.TCPSocket == nil {
		t.Fatal("XDS container: expected readiness probe with TCPSocket")
	}
	if cp.ReadinessProbe.TCPSocket.Port.IntValue() != 9002 {
		t.Fatalf("XDS container: expected readiness probe port 9002, got %d", cp.ReadinessProbe.TCPSocket.Port.IntValue())
	}

	if cp.StartupProbe == nil || cp.StartupProbe.TCPSocket == nil {
		t.Fatal("XDS container: expected startup probe with TCPSocket")
	}
	if cp.StartupProbe.TCPSocket.Port.IntValue() != 9002 {
		t.Fatalf("XDS container: expected startup probe port 9002, got %d", cp.StartupProbe.TCPSocket.Port.IntValue())
	}

	// DNS container: no probes
	dns := containers[2]
	if dns.LivenessProbe != nil || dns.ReadinessProbe != nil || dns.StartupProbe != nil {
		t.Fatal("DNS container: expected no probes")
	}
}

func TestGenDeploySpec_ContainerPorts(t *testing.T) {
	deploy := genDeploySpec("test-ns", "img:latest", "")
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(containers))
	}

	// VPN container: 1 port named PortNameTCP on 10801/TCP
	vpn := containers[0]
	if len(vpn.Ports) != 1 {
		t.Fatalf("VPN container: expected 1 port, got %d", len(vpn.Ports))
	}
	if vpn.Ports[0].Name != config.PortNameTCP {
		t.Fatalf("VPN container: expected port name %q, got %q", config.PortNameTCP, vpn.Ports[0].Name)
	}
	if vpn.Ports[0].ContainerPort != 10801 {
		t.Fatalf("VPN container: expected containerPort 10801, got %d", vpn.Ports[0].ContainerPort)
	}
	if vpn.Ports[0].Protocol != v1.ProtocolTCP {
		t.Fatalf("VPN container: expected protocol TCP, got %q", vpn.Ports[0].Protocol)
	}

	// Control Plane container: 1 port (PortNameEnvoy on 9002/TCP)
	cp := containers[1]
	if len(cp.Ports) != 1 {
		t.Fatalf("XDS container: expected 1 port, got %d", len(cp.Ports))
	}
	if cp.Ports[0].Name != config.PortNameEnvoy {
		t.Fatalf("XDS container: expected port name %q, got %q", config.PortNameEnvoy, cp.Ports[0].Name)
	}
	if cp.Ports[0].ContainerPort != 9002 {
		t.Fatalf("XDS container: expected containerPort 9002, got %d", cp.Ports[0].ContainerPort)
	}
	if cp.Ports[0].Protocol != v1.ProtocolTCP {
		t.Fatalf("XDS container: expected protocol TCP, got %q", cp.Ports[0].Protocol)
	}

	// DNS container: 1 port (PortNameDNS on 53/UDP)
	dns := containers[2]
	if len(dns.Ports) != 1 {
		t.Fatalf("DNS container: expected 1 port, got %d", len(dns.Ports))
	}
	if dns.Ports[0].Name != config.PortNameDNS {
		t.Fatalf("DNS container: expected port name %q, got %q", config.PortNameDNS, dns.Ports[0].Name)
	}
	if dns.Ports[0].ContainerPort != 53 {
		t.Fatalf("DNS container: expected containerPort 53, got %d", dns.Ports[0].ContainerPort)
	}
	if dns.Ports[0].Protocol != v1.ProtocolUDP {
		t.Fatalf("DNS container: expected protocol UDP, got %q", dns.Ports[0].Protocol)
	}
}

func TestGenDeploySpec_SecurityContext(t *testing.T) {
	deploy := genDeploySpec("test-ns", "img:latest", "")
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(containers))
	}

	vpn := containers[0]

	// VPN container must have a SecurityContext
	if vpn.SecurityContext == nil {
		t.Fatal("VPN container: expected non-nil SecurityContext")
	}

	// Privileged must be false
	if vpn.SecurityContext.Privileged == nil || *vpn.SecurityContext.Privileged != false {
		t.Fatalf("VPN container: expected Privileged=false, got %v", vpn.SecurityContext.Privileged)
	}

	// Capabilities must not include NET_ADMIN
	if vpn.SecurityContext.Capabilities == nil {
		t.Fatal("VPN container: expected non-nil Capabilities")
	}
	for _, cap := range vpn.SecurityContext.Capabilities.Add {
		if cap == "NET_ADMIN" {
			t.Fatal("VPN container: must not have NET_ADMIN capability")
		}
	}

	// XDS and DNS containers should not have a privileged security context either
	for _, c := range containers[1:] {
		if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
			t.Fatalf("container %q: expected Privileged to not be true", c.Name)
		}
	}
}

// Ensure the returned types satisfy their respective interfaces for type safety.
var _ *v1.ServiceAccount = genServiceAccount("x")
var _ *v1.Service = genService("x")
var _ *v1.Secret = genSecret("x", nil, nil, nil)
var _ *appsv1.Deployment = genDeploySpec("x", "", "")

func TestWaitPodReady_Timeout(t *testing.T) {
	namespace := "test-ns"
	labelSelector := "app=test-timeout"

	clientset := fake.NewSimpleClientset()
	podClient := clientset.CoreV1().Pods(namespace)

	// Create a pod that is Pending (not ready) — it will never transition.
	pendingPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pending-pod",
			Namespace: namespace,
			Labels:    map[string]string{"app": "test-timeout"},
		},
		Status: v1.PodStatus{
			Phase: v1.PodPending,
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodReady,
					Status: v1.ConditionFalse,
				},
			},
		},
	}
	_, err := podClient.Create(context.Background(), pendingPod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create pending pod: %v", err)
	}

	// Use an already-cancelled context so WaitPodReady returns immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = WaitPodReady(ctx, podClient, labelSelector, "")
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, config.ErrTrafficManagerTimeout) {
		t.Fatalf("expected error to wrap ErrTrafficManagerTimeout, got %q", err.Error())
	}
}

func TestWaitPodReady_TransitionsToReady(t *testing.T) {
	namespace := "test-ns"
	labelSelector := "app=test-transition"

	clientset := fake.NewSimpleClientset()
	podClient := clientset.CoreV1().Pods(namespace)

	// Create a pod that starts as Pending (not ready).
	pendingPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "transitioning-pod",
			Namespace: namespace,
			Labels:    map[string]string{"app": "test-transition"},
		},
		Status: v1.PodStatus{
			Phase: v1.PodPending,
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodReady,
					Status: v1.ConditionFalse,
				},
			},
		},
	}
	_, err := podClient.Create(context.Background(), pendingPod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create pending pod: %v", err)
	}

	// After a short delay, update the pod to be Ready.
	go func() {
		time.Sleep(500 * time.Millisecond)
		readyPod := pendingPod.DeepCopy()
		readyPod.Status.Phase = v1.PodRunning
		readyPod.Status.Conditions = []v1.PodCondition{
			{
				Type:   v1.PodReady,
				Status: v1.ConditionTrue,
			},
		}
		_, updateErr := podClient.UpdateStatus(context.Background(), readyPod, metav1.UpdateOptions{})
		if updateErr != nil {
			// Cannot t.Fatal from goroutine; the test will fail on timeout instead.
			t.Errorf("failed to update pod status: %v", updateErr)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = WaitPodReady(ctx, podClient, labelSelector, "")
	if err != nil {
		t.Fatalf("expected pod to become ready, got error: %v", err)
	}
}

// TestCreateOutboundPod_RecreatesWhenDeploymentMissing reproduces the fargate
// proxy-after-uninstall race seen in CI (TestFunctions/04-proxy-mesh-fargate):
// `kubevpn uninstall` deletes the traffic-manager Deployment with
// GracePeriodSeconds=0, but its pod can briefly linger via cascade-GC lag,
// still reporting Running with no deletionTimestamp. DetectPodExists then
// returns true, so the old createOutboundPod skipped creation entirely —
// leaving the next connect step (UpgradeDeploy → Deployment Get) to fail with
// `deployments.apps "kubevpn-traffic-manager" not found` (exit 50), which tore
// down the connection and cascaded into the phase's pingPodIP/health timeouts.
//
// The fix requires the Deployment (UpgradeDeploy's real dependency) to exist
// before treating the manager as installed, so a lingering pod no longer masks
// a missing Deployment.
func TestCreateOutboundPod_RecreatesWhenDeploymentMissing(t *testing.T) {
	const ns = "default"

	// A ready traffic-manager pod that outlived its (already deleted) Deployment.
	lingeringPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager + "-lingering",
			Namespace: ns,
			Labels:    map[string]string{"app": config.ConfigMapPodTrafficManager},
		},
		Status: v1.PodStatus{
			Phase:      v1.PodRunning,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			ContainerStatuses: []v1.ContainerStatus{
				{Name: "vpn", Ready: true, State: v1.ContainerState{Running: &v1.ContainerStateRunning{}}},
			},
		},
	}
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		lingeringPod,
	)

	// Precondition: the lingering pod makes the manager look present...
	exists, err := util.DetectPodExists(context.Background(), clientset, ns)
	if err != nil {
		t.Fatalf("DetectPodExists: %v", err)
	}
	if !exists {
		t.Fatal("precondition failed: lingering pod should make DetectPodExists report the manager present")
	}
	// ...while its Deployment is gone.
	if _, err := clientset.AppsV1().Deployments(ns).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{}); err == nil {
		t.Fatal("precondition failed: traffic-manager Deployment should not exist yet")
	}

	// Bound the call: with the fix WaitPodReady finds the ready pod and returns
	// immediately; the timeout only guards against a regression hanging CI.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := createOutboundPod(ctx, clientset, ns, "ghcr.io/test/kubevpn:latest", ""); err != nil {
		t.Fatalf("createOutboundPod returned error: %v", err)
	}

	// The fix must have re-created the Deployment despite the lingering pod.
	if _, err := clientset.AppsV1().Deployments(ns).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{}); err != nil {
		t.Fatalf("expected traffic-manager Deployment to be re-created, got: %v", err)
	}
}
