package handler

import (
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
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
	if len(role.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(role.Rules))
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
	svc := genService(namespace, "tcp-10801", "tcp-9002", "tcp-80", "udp-53")

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

	// 4 ports
	if len(svc.Spec.Ports) != 4 {
		t.Fatalf("expected 4 ports, got %d", len(svc.Spec.Ports))
	}

	type portCase struct {
		name     string
		port     int32
		protocol v1.Protocol
	}
	expected := []portCase{
		{"tcp-10801", 10801, v1.ProtocolTCP},
		{"tcp-9002", 9002, v1.ProtocolTCP},
		{"tcp-80", 80, v1.ProtocolTCP},
		{"udp-53", 53, v1.ProtocolUDP},
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
	tcp10801 := "tcp-10801"
	tcp9002 := "tcp-9002"
	udp53 := "udp-53"
	tcp80 := "tcp-80"
	image := "docker.io/naison/kubevpn:test"
	imagePullSecret := "my-pull-secret"

	deploy := genDeploySpec(namespace, tcp10801, tcp9002, udp53, tcp80, image, imagePullSecret)

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
	if cp.Name != config.ContainerSidecarControlPlane {
		t.Fatalf("expected container[1] name %q, got %q", config.ContainerSidecarControlPlane, cp.Name)
	}
	if cp.Image != image {
		t.Fatalf("expected container[1] image %q, got %q", image, cp.Image)
	}
	if len(cp.Ports) != 2 {
		t.Fatalf("expected container[1] to have 2 ports, got %d", len(cp.Ports))
	}
	if cp.Ports[0].ContainerPort != 9002 || cp.Ports[1].ContainerPort != 53 {
		t.Fatalf("expected container[1] ports 9002 and 53, got %d and %d", cp.Ports[0].ContainerPort, cp.Ports[1].ContainerPort)
	}
	if cp.LivenessProbe == nil || cp.ReadinessProbe == nil || cp.StartupProbe == nil {
		t.Fatal("expected container[1] to have all three probes")
	}

	// Container 2: Webhook
	wh := containers[2]
	if wh.Name != config.ContainerSidecarWebhook {
		t.Fatalf("expected container[2] name %q, got %q", config.ContainerSidecarWebhook, wh.Name)
	}
	if wh.Image != image {
		t.Fatalf("expected container[2] image %q, got %q", image, wh.Image)
	}
	if len(wh.Ports) != 1 || wh.Ports[0].ContainerPort != 80 {
		t.Fatalf("expected container[2] port 80, got %v", wh.Ports)
	}
	if wh.LivenessProbe == nil || wh.ReadinessProbe == nil || wh.StartupProbe == nil {
		t.Fatal("expected container[2] to have all three probes")
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
	deploy := genDeploySpec("ns", "tcp-10801", "tcp-9002", "udp-53", "tcp-80", "img:latest", "")

	if len(deploy.Spec.Template.Spec.ImagePullSecrets) != 0 {
		t.Fatalf("expected no imagePullSecrets when name is empty, got %d", len(deploy.Spec.Template.Spec.ImagePullSecrets))
	}
}

func TestGenMutatingWebhookConfiguration(t *testing.T) {
	namespace := "test-ns"
	crt := []byte("fake-ca-cert")

	mwc := genMutatingWebhookConfiguration(namespace, crt)

	// Object metadata
	expectedName := config.ConfigMapPodTrafficManager + "." + namespace
	if mwc.Name != expectedName {
		t.Fatalf("expected name %q, got %q", expectedName, mwc.Name)
	}
	if mwc.Namespace != namespace {
		t.Fatalf("expected namespace %q, got %q", namespace, mwc.Namespace)
	}

	// Exactly one webhook
	if len(mwc.Webhooks) != 1 {
		t.Fatalf("expected 1 webhook, got %d", len(mwc.Webhooks))
	}
	wh := mwc.Webhooks[0]

	// Webhook name
	expectedWebhookName := config.ConfigMapPodTrafficManager + ".naison.io"
	if wh.Name != expectedWebhookName {
		t.Fatalf("expected webhook name %q, got %q", expectedWebhookName, wh.Name)
	}

	// CA bundle
	if string(wh.ClientConfig.CABundle) != string(crt) {
		t.Fatalf("expected CABundle %q, got %q", crt, wh.ClientConfig.CABundle)
	}

	// Service reference
	svcRef := wh.ClientConfig.Service
	if svcRef == nil {
		t.Fatal("expected non-nil service reference")
	}
	if svcRef.Namespace != namespace {
		t.Fatalf("expected service namespace %q, got %q", namespace, svcRef.Namespace)
	}
	if svcRef.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected service name %q, got %q", config.ConfigMapPodTrafficManager, svcRef.Name)
	}
	if svcRef.Path == nil || *svcRef.Path != "/pods" {
		t.Fatalf("expected service path %q, got %v", "/pods", svcRef.Path)
	}
	if svcRef.Port == nil || *svcRef.Port != 80 {
		t.Fatalf("expected service port 80, got %v", svcRef.Port)
	}

	// Rules
	if len(wh.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(wh.Rules))
	}
	rule := wh.Rules[0]
	if len(rule.Operations) != 2 {
		t.Fatalf("expected 2 operations, got %d", len(rule.Operations))
	}
	if rule.Operations[0] != admissionv1.Create || rule.Operations[1] != admissionv1.Delete {
		t.Fatalf("expected operations [CREATE, DELETE], got %v", rule.Operations)
	}
	if len(rule.Rule.APIGroups) != 1 || rule.Rule.APIGroups[0] != "" {
		t.Fatalf("expected APIGroups [\"\"], got %v", rule.Rule.APIGroups)
	}
	if len(rule.Rule.APIVersions) != 1 || rule.Rule.APIVersions[0] != "v1" {
		t.Fatalf("expected APIVersions [\"v1\"], got %v", rule.Rule.APIVersions)
	}
	if len(rule.Rule.Resources) != 1 || rule.Rule.Resources[0] != "pods" {
		t.Fatalf("expected Resources [\"pods\"], got %v", rule.Rule.Resources)
	}
	if rule.Rule.Scope == nil || *rule.Rule.Scope != admissionv1.NamespacedScope {
		t.Fatalf("expected scope Namespaced, got %v", rule.Rule.Scope)
	}

	// Failure policy
	if wh.FailurePolicy == nil || *wh.FailurePolicy != admissionv1.Ignore {
		t.Fatalf("expected failurePolicy Ignore, got %v", wh.FailurePolicy)
	}

	// SideEffects
	if wh.SideEffects == nil || *wh.SideEffects != admissionv1.SideEffectClassNone {
		t.Fatalf("expected sideEffects None, got %v", wh.SideEffects)
	}

	// TimeoutSeconds
	if wh.TimeoutSeconds == nil || *wh.TimeoutSeconds != 15 {
		t.Fatalf("expected timeoutSeconds 15, got %v", wh.TimeoutSeconds)
	}

	// AdmissionReviewVersions
	expectedVersions := []string{"v1", "v1beta1"}
	if len(wh.AdmissionReviewVersions) != len(expectedVersions) {
		t.Fatalf("expected %d admissionReviewVersions, got %d", len(expectedVersions), len(wh.AdmissionReviewVersions))
	}
	for i, v := range expectedVersions {
		if wh.AdmissionReviewVersions[i] != v {
			t.Fatalf("expected admissionReviewVersions[%d]=%q, got %q", i, v, wh.AdmissionReviewVersions[i])
		}
	}

	// ReinvocationPolicy
	if wh.ReinvocationPolicy == nil || *wh.ReinvocationPolicy != admissionv1.NeverReinvocationPolicy {
		t.Fatalf("expected reinvocationPolicy Never, got %v", wh.ReinvocationPolicy)
	}

	// NamespaceSelector: non-kubevpn namespace should have label selector
	if wh.NamespaceSelector == nil {
		t.Fatal("expected non-nil namespaceSelector for non-kubevpn namespace")
	}
	if wh.NamespaceSelector.MatchLabels["ns"] != namespace {
		t.Fatalf("expected namespaceSelector matchLabels ns=%q, got %v", namespace, wh.NamespaceSelector.MatchLabels)
	}

	// NamespaceSelector: kubevpn namespace should be nil
	mwcKubevpn := genMutatingWebhookConfiguration(config.DefaultNamespaceKubevpn, crt)
	if mwcKubevpn.Webhooks[0].NamespaceSelector != nil {
		t.Fatalf("expected nil namespaceSelector for kubevpn namespace, got %v", mwcKubevpn.Webhooks[0].NamespaceSelector)
	}
}

func TestGenDeploySpec_Probes(t *testing.T) {
	deploy := genDeploySpec("test-ns", "tcp-10801", "tcp-9002", "udp-53", "tcp-80", "img:latest", "")
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(containers))
	}

	// VPN container: TCP probes on port 10801
	vpn := containers[0]
	if vpn.LivenessProbe == nil || vpn.LivenessProbe.TCPSocket == nil {
		t.Fatal("VPN container: expected liveness probe with TCPSocket")
	}
	if vpn.LivenessProbe.TCPSocket.Port.IntValue() != 10801 {
		t.Fatalf("VPN container: expected liveness probe port 10801, got %d", vpn.LivenessProbe.TCPSocket.Port.IntValue())
	}
	if vpn.LivenessProbe.InitialDelaySeconds != 5 || vpn.LivenessProbe.PeriodSeconds != 15 || vpn.LivenessProbe.FailureThreshold != 3 {
		t.Fatalf("VPN container: unexpected liveness probe timing: initial=%d period=%d failure=%d",
			vpn.LivenessProbe.InitialDelaySeconds, vpn.LivenessProbe.PeriodSeconds, vpn.LivenessProbe.FailureThreshold)
	}

	if vpn.ReadinessProbe == nil || vpn.ReadinessProbe.TCPSocket == nil {
		t.Fatal("VPN container: expected readiness probe with TCPSocket")
	}
	if vpn.ReadinessProbe.TCPSocket.Port.IntValue() != 10801 {
		t.Fatalf("VPN container: expected readiness probe port 10801, got %d", vpn.ReadinessProbe.TCPSocket.Port.IntValue())
	}
	if vpn.ReadinessProbe.InitialDelaySeconds != 3 || vpn.ReadinessProbe.PeriodSeconds != 10 || vpn.ReadinessProbe.FailureThreshold != 3 {
		t.Fatalf("VPN container: unexpected readiness probe timing: initial=%d period=%d failure=%d",
			vpn.ReadinessProbe.InitialDelaySeconds, vpn.ReadinessProbe.PeriodSeconds, vpn.ReadinessProbe.FailureThreshold)
	}

	if vpn.StartupProbe == nil || vpn.StartupProbe.TCPSocket == nil {
		t.Fatal("VPN container: expected startup probe with TCPSocket")
	}
	if vpn.StartupProbe.TCPSocket.Port.IntValue() != 10801 {
		t.Fatalf("VPN container: expected startup probe port 10801, got %d", vpn.StartupProbe.TCPSocket.Port.IntValue())
	}
	if vpn.StartupProbe.InitialDelaySeconds != 1 || vpn.StartupProbe.PeriodSeconds != 2 || vpn.StartupProbe.FailureThreshold != 15 {
		t.Fatalf("VPN container: unexpected startup probe timing: initial=%d period=%d failure=%d",
			vpn.StartupProbe.InitialDelaySeconds, vpn.StartupProbe.PeriodSeconds, vpn.StartupProbe.FailureThreshold)
	}

	// Control Plane container: TCP probes on port 9002
	cp := containers[1]
	if cp.LivenessProbe == nil || cp.LivenessProbe.TCPSocket == nil {
		t.Fatal("ControlPlane container: expected liveness probe with TCPSocket")
	}
	if cp.LivenessProbe.TCPSocket.Port.IntValue() != 9002 {
		t.Fatalf("ControlPlane container: expected liveness probe port 9002, got %d", cp.LivenessProbe.TCPSocket.Port.IntValue())
	}
	if cp.LivenessProbe.InitialDelaySeconds != 5 || cp.LivenessProbe.PeriodSeconds != 15 || cp.LivenessProbe.FailureThreshold != 3 {
		t.Fatalf("ControlPlane container: unexpected liveness probe timing: initial=%d period=%d failure=%d",
			cp.LivenessProbe.InitialDelaySeconds, cp.LivenessProbe.PeriodSeconds, cp.LivenessProbe.FailureThreshold)
	}

	if cp.ReadinessProbe == nil || cp.ReadinessProbe.TCPSocket == nil {
		t.Fatal("ControlPlane container: expected readiness probe with TCPSocket")
	}
	if cp.ReadinessProbe.TCPSocket.Port.IntValue() != 9002 {
		t.Fatalf("ControlPlane container: expected readiness probe port 9002, got %d", cp.ReadinessProbe.TCPSocket.Port.IntValue())
	}

	if cp.StartupProbe == nil || cp.StartupProbe.TCPSocket == nil {
		t.Fatal("ControlPlane container: expected startup probe with TCPSocket")
	}
	if cp.StartupProbe.TCPSocket.Port.IntValue() != 9002 {
		t.Fatalf("ControlPlane container: expected startup probe port 9002, got %d", cp.StartupProbe.TCPSocket.Port.IntValue())
	}

	// Webhook container: HTTP probes on port 80 with HTTPS scheme
	wh := containers[2]
	if wh.LivenessProbe == nil || wh.LivenessProbe.HTTPGet == nil {
		t.Fatal("Webhook container: expected liveness probe with HTTPGet")
	}
	if wh.LivenessProbe.HTTPGet.Port.IntValue() != 80 {
		t.Fatalf("Webhook container: expected liveness probe port 80, got %d", wh.LivenessProbe.HTTPGet.Port.IntValue())
	}
	if wh.LivenessProbe.HTTPGet.Path != "/readyz" {
		t.Fatalf("Webhook container: expected liveness probe path /readyz, got %q", wh.LivenessProbe.HTTPGet.Path)
	}
	if wh.LivenessProbe.HTTPGet.Scheme != v1.URISchemeHTTPS {
		t.Fatalf("Webhook container: expected liveness probe scheme HTTPS, got %q", wh.LivenessProbe.HTTPGet.Scheme)
	}

	if wh.ReadinessProbe == nil || wh.ReadinessProbe.HTTPGet == nil {
		t.Fatal("Webhook container: expected readiness probe with HTTPGet")
	}
	if wh.ReadinessProbe.HTTPGet.Port.IntValue() != 80 {
		t.Fatalf("Webhook container: expected readiness probe port 80, got %d", wh.ReadinessProbe.HTTPGet.Port.IntValue())
	}
	if wh.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Fatalf("Webhook container: expected readiness probe path /readyz, got %q", wh.ReadinessProbe.HTTPGet.Path)
	}
	if wh.ReadinessProbe.HTTPGet.Scheme != v1.URISchemeHTTPS {
		t.Fatalf("Webhook container: expected readiness probe scheme HTTPS, got %q", wh.ReadinessProbe.HTTPGet.Scheme)
	}

	if wh.StartupProbe == nil || wh.StartupProbe.HTTPGet == nil {
		t.Fatal("Webhook container: expected startup probe with HTTPGet")
	}
	if wh.StartupProbe.HTTPGet.Port.IntValue() != 80 {
		t.Fatalf("Webhook container: expected startup probe port 80, got %d", wh.StartupProbe.HTTPGet.Port.IntValue())
	}
	if wh.StartupProbe.HTTPGet.Path != "/readyz" {
		t.Fatalf("Webhook container: expected startup probe path /readyz, got %q", wh.StartupProbe.HTTPGet.Path)
	}
	if wh.StartupProbe.HTTPGet.Scheme != v1.URISchemeHTTPS {
		t.Fatalf("Webhook container: expected startup probe scheme HTTPS, got %q", wh.StartupProbe.HTTPGet.Scheme)
	}
}

func TestGenDeploySpec_ContainerPorts(t *testing.T) {
	deploy := genDeploySpec("test-ns",
		config.PortNameTCP, config.PortNameEnvoy, config.PortNameDNS, config.PortNameHTTP,
		"img:latest", "")
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

	// Control Plane container: 2 ports (PortNameEnvoy on 9002/TCP, PortNameDNS on 53/UDP)
	cp := containers[1]
	if len(cp.Ports) != 2 {
		t.Fatalf("ControlPlane container: expected 2 ports, got %d", len(cp.Ports))
	}
	if cp.Ports[0].Name != config.PortNameEnvoy {
		t.Fatalf("ControlPlane container: expected port[0] name %q, got %q", config.PortNameEnvoy, cp.Ports[0].Name)
	}
	if cp.Ports[0].ContainerPort != 9002 {
		t.Fatalf("ControlPlane container: expected port[0] containerPort 9002, got %d", cp.Ports[0].ContainerPort)
	}
	if cp.Ports[0].Protocol != v1.ProtocolTCP {
		t.Fatalf("ControlPlane container: expected port[0] protocol TCP, got %q", cp.Ports[0].Protocol)
	}
	if cp.Ports[1].Name != config.PortNameDNS {
		t.Fatalf("ControlPlane container: expected port[1] name %q, got %q", config.PortNameDNS, cp.Ports[1].Name)
	}
	if cp.Ports[1].ContainerPort != 53 {
		t.Fatalf("ControlPlane container: expected port[1] containerPort 53, got %d", cp.Ports[1].ContainerPort)
	}
	if cp.Ports[1].Protocol != v1.ProtocolUDP {
		t.Fatalf("ControlPlane container: expected port[1] protocol UDP, got %q", cp.Ports[1].Protocol)
	}

	// Webhook container: 1 port named PortNameHTTP on 80/TCP
	wh := containers[2]
	if len(wh.Ports) != 1 {
		t.Fatalf("Webhook container: expected 1 port, got %d", len(wh.Ports))
	}
	if wh.Ports[0].Name != config.PortNameHTTP {
		t.Fatalf("Webhook container: expected port name %q, got %q", config.PortNameHTTP, wh.Ports[0].Name)
	}
	if wh.Ports[0].ContainerPort != 80 {
		t.Fatalf("Webhook container: expected containerPort 80, got %d", wh.Ports[0].ContainerPort)
	}
	if wh.Ports[0].Protocol != v1.ProtocolTCP {
		t.Fatalf("Webhook container: expected protocol TCP, got %q", wh.Ports[0].Protocol)
	}
}

func TestGenDeploySpec_SecurityContext(t *testing.T) {
	deploy := genDeploySpec("test-ns", "tcp-10801", "tcp-9002", "udp-53", "tcp-80", "img:latest", "")
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

	// ControlPlane and Webhook containers should not have a privileged security context either
	for _, c := range containers[1:] {
		if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
			t.Fatalf("container %q: expected Privileged to not be true", c.Name)
		}
	}
}

// Ensure the returned types satisfy their respective interfaces for type safety.
var _ *v1.ServiceAccount = genServiceAccount("x")
var _ *v1.Service = genService("x", "", "", "", "")
var _ *v1.Secret = genSecret("x", nil, nil, nil)
var _ *appsv1.Deployment = genDeploySpec("x", "", "", "", "", "", "")
