package util

import (
	"context"
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
)

// parseServiceCIDRFromError extracts the Service CIDR the API server embeds in the
// rejection message for an invalid ClusterIP. Dry-run returns the authoritative range on
// both standard and managed control planes; GKE's real-create message carries no CIDR.
func TestParseServiceCIDRFromError(t *testing.T) {
	tests := []struct {
		name    string
		errMsg  string
		want    string
		wantOk  bool
	}{
		{
			name:   "GKE server-side dry-run message",
			errMsg: `The Service "foo-svc-x" is invalid: spec.clusterIPs: Invalid value: ["0.0.0.0"]: failed to allocate IP 0.0.0.0: the provided IP (0.0.0.0) is not in the valid range. The range of valid IPs is 34.118.224.0/20`,
			want:   "34.118.224.0/20",
			wantOk: true,
		},
		{
			name:   "standard kubeadm/minikube message",
			errMsg: `The Service "foo-svc-y" is invalid: spec.clusterIPs: Invalid value: ["0.0.0.0"]: provided IP is not in the valid IP range. The range of valid IPs is 10.96.0.0/12`,
			want:   "10.96.0.0/12",
			wantOk: true,
		},
		{
			name:   "keyword 'valid IP range is'",
			errMsg: `... the valid IP range is 192.168.0.0/16`,
			want:   "192.168.0.0/16",
			wantOk: true,
		},
		{
			// GKE real-create wording — no CIDR anywhere, so nothing is parseable.
			name:   "GKE real-create message carries no CIDR",
			errMsg: `The Service "foo-svc-z" is invalid: spec.clusterIPs: Invalid value: ["0.0.0.0"]: failed to allocate IP 0.0.0.0: the provided network does not match the current range`,
			wantOk: false,
		},
		{
			name:   "totally unrelated error",
			errMsg: "services is forbidden: User cannot create resource",
			wantOk: false,
		},
		{
			name:   "empty string",
			errMsg: "",
			wantOk: false,
		},
		// IPv6 range.
		{
			name:   "IPv6 service range",
			errMsg: `The range of valid IPs is fd00::/108`,
			want:   "fd00::/108",
			wantOk: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseServiceCIDRFromError(tt.errMsg)
			if ok != tt.wantOk {
				t.Fatalf("parseServiceCIDRFromError ok = %v, want %v (got %v)", ok, tt.wantOk, got)
			}
			if !tt.wantOk {
				return
			}
			if got == nil || got.String() != tt.want {
				t.Errorf("parseServiceCIDRFromError = %v, want %s", got, tt.want)
			}
		})
	}
}

// TestGetServiceCIDRByCreateService_DryRunDoesNotPersist verifies the probe runs as a
// server-side dry-run: against a fake clientset (which has no admission, so Create succeeds
// and returns no CIDR) NO Service is ever persisted — proving dry-run leaves no trace. On a
// real API server the dry-run is rejected with the valid range parsed by
// parseServiceCIDRFromError (covered by the integration TestByCreateSvc against a cluster).
func TestGetServiceCIDRByCreateService_DryRunDoesNotPersist(t *testing.T) {
	const ns = "kubevpn"
	cs := fake.NewSimpleClientset()
	_, err := GetServiceCIDRByCreateService(context.Background(), cs.CoreV1().Services(ns))
	if err == nil {
		t.Fatal("expected an error: fake clientset has no admission, so dry-run returns no CIDR")
	}
	list, lErr := cs.CoreV1().Services(ns).List(context.Background(), metav1.ListOptions{})
	if lErr != nil {
		t.Fatalf("list services: %v", lErr)
	}
	for _, s := range list.Items {
		if len(s.Name) >= len("foo-svc-") && s.Name[:len("foo-svc-")] == "foo-svc-" {
			t.Errorf("dry-run must not persist a Service, found %q", s.Name)
		}
	}
}

// cidrSet collects the string form of a CIDR slice into a set for order-independent asserts.
func cidrSet(cidrs []*net.IPNet) sets.Set[string] {
	s := sets.New[string]()
	for _, c := range cidrs {
		if c != nil {
			s.Insert(c.String())
		}
	}
	return s
}

func TestInferredCIDRFromIP(t *testing.T) {
	tests := []struct {
		ip   string
		want string
	}{
		{"10.96.0.1", "10.96.0.0/24"},   // IPv4 -> /24, normalized to network address
		{"10.107.5.20", "10.107.5.0/24"},
		{"fd00::5", "fd00::/64"},         // IPv6 -> /64
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", tt.ip)
		}
		got := inferredCIDRFromIP(ip)
		if got == nil || got.String() != tt.want {
			t.Errorf("inferredCIDRFromIP(%s) = %v, want %s", tt.ip, got, tt.want)
		}
	}
}

func TestGetServiceCIDRFromService(t *testing.T) {
	const ns = "default"
	svc := func(name, clusterIP string, clusterIPs ...string) *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       corev1.ServiceSpec{ClusterIP: clusterIP, ClusterIPs: clusterIPs},
		}
	}
	cs := fake.NewSimpleClientset(
		svc("kubernetes", "10.96.0.1"),          // v4 -> 10.96.0.0/24 ┐ merge to common
		svc("api", "10.107.5.20"),               // v4 -> 10.107.5.0/24 ┘ supernet 10.96.0.0/12
		svc("dup", "10.96.0.42"),                // same /24 as kubernetes -> deduped
		svc("headless", corev1.ClusterIPNone),   // "None" -> skipped
		svc("empty", ""),                        // empty -> skipped
		svc("dual", "10.96.0.1", "10.96.0.1", "fd00::abcd"), // dual-stack -> v4 dedup + v6 /64
	)

	got, err := GetServiceCIDRFromService(context.Background(), cs, ns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	set := cidrSet(got)
	// The two v4 /24s coalesce into their bounded common supernet (10.96.0.0/12); the lone
	// v6 /64 has no sibling to merge with and is preserved. See mergeToSupernet.
	want := sets.New[string]("10.96.0.0/12", "fd00::/64")
	if !set.Equal(want) {
		t.Errorf("GetServiceCIDRFromService = %v, want %v", set.UnsortedList(), want.UnsortedList())
	}
}

func TestGetPodCIDRFromPod_MaskAndSkips(t *testing.T) {
	const ns = "default"
	pod := func(name string, hostNetwork bool, podIP string, podIPs ...string) *corev1.Pod {
		var ips []corev1.PodIP
		for _, p := range podIPs {
			ips = append(ips, corev1.PodIP{IP: p})
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       corev1.PodSpec{HostNetwork: hostNetwork},
			Status:     corev1.PodStatus{PodIP: podIP, PodIPs: ips},
		}
	}
	cs := fake.NewSimpleClientset(
		pod("app", false, "10.244.3.7"),                  // v4 -> 10.244.3.0/24 (guards /16 -> /24)
		pod("host", true, "192.168.1.10"),                // HostNetwork -> skipped
		pod("dual", false, "10.244.3.9", "fd00:10::5"),   // same /24 (dedup) + v6 /64
	)

	got, err := GetPodCIDRFromPod(context.Background(), cs, ns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	set := cidrSet(got)
	if !set.Has("10.244.3.0/24") {
		t.Errorf("expected 10.244.3.0/24 (/24 mask), got %v", set.UnsortedList())
	}
	if !set.Has("fd00:10::/64") {
		t.Errorf("expected fd00:10::/64, got %v", set.UnsortedList())
	}
	if set.Has("192.168.1.0/24") {
		t.Errorf("HostNetwork pod IP must be skipped, got %v", set.UnsortedList())
	}
}

// TestDetectServiceCIDRs verifies the shared service-CIDR union includes Strategy 5
// (infer-from-Services), which is the reporting fix: previously its result was appended after
// the "Detected service CIDR" line was printed. Strategy 4 (the rejected-Service error trick)
// contributes nothing against a fake cluster — fake Create does not reject ClusterIP 0.0.0.0,
// so unlike a real API server it returns no CIDR — leaving the inferred Service /24 in the union.
func TestDetectServiceCIDRs(t *testing.T) {
	const ns = "default"
	cs := fake.NewSimpleClientset(
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: ns},
			Spec:       corev1.ServiceSpec{ClusterIP: "10.96.0.1"}, // inferred -> 10.96.0.0/24
		},
	)

	got := cidrSet(detectServiceCIDRs(context.Background(), cs, ns))
	if !got.Has("10.96.0.0/24") {
		t.Errorf("detectServiceCIDRs must include the inferred Service CIDR 10.96.0.0/24, got %v", got.UnsortedList())
	}
}

// TestGetCIDR_NoProbePod verifies the client/data-plane entry point runs entirely pod-free:
// it returns the component-flag CIDRs, the Pod CIDR inferred from live Pod IPs, and the
// Service CIDR inferred from Services — without ever creating a probe pod (which previously
// blocked ~15s on WaitPod). Running to completion against a fake cluster (where a probe pod
// would never reach Running) is itself the proof that no probe pod is created.
func TestGetCIDR_NoProbePod(t *testing.T) {
	const ns = "kubevpn"
	cs := fake.NewSimpleClientset(
		// kube-system control-plane pod exposing component flags.
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-apiserver", Namespace: metav1.NamespaceSystem},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "kube-apiserver",
				Command: []string{
					"kube-apiserver",
					"--service-cluster-ip-range=10.96.0.0/12",
					"--cluster-cidr=10.244.0.0/16",
				},
			}}},
		},
		// A Service in the namespace → inferred service /24.
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns},
			Spec:       corev1.ServiceSpec{ClusterIP: "10.96.0.100"},
		},
		// A business Pod in the namespace → inferred pod /24.
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
			Status:     corev1.PodStatus{PodIP: "10.244.1.5"},
		},
	)

	got := cidrSet(GetCIDR(context.Background(), cs, ns))
	for _, want := range []string{
		"10.96.0.0/12",  // component flag: service-cluster-ip-range
		"10.244.0.0/16", // component flag: cluster-cidr
		"10.244.1.0/24", // inferred from pod IP 10.244.1.5
		"10.96.0.0/24",  // inferred from Service ClusterIP 10.96.0.100
	} {
		if !got.Has(want) {
			t.Errorf("GetCIDR missing %s, got %v", want, got.UnsortedList())
		}
	}
}

// TestGetClusterCIDRNoProbePod_Composes wires the three pod-free strategies together
// against a fake cluster and asserts the combined result contains the component-flag
// CIDRs, the listed-Service /24s, and the pod /24s.
func TestGetClusterCIDRNoProbePod_Composes(t *testing.T) {
	const ns = "kubevpn"
	cs := fake.NewSimpleClientset(
		// kube-system control-plane pod with component flags (Strategy 1).
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-apiserver", Namespace: metav1.NamespaceSystem},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "kube-apiserver",
				Command: []string{
					"kube-apiserver",
					"--service-cluster-ip-range=10.96.0.0/12",
				},
			}}},
		},
		// Service in the manager namespace (Strategy 5 - list Services).
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "traffic-manager", Namespace: ns},
			Spec:       corev1.ServiceSpec{ClusterIP: "10.96.0.100"},
		},
		// Pod in the manager namespace (Strategy 6 - infer from pod IP).
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns},
			Status:     corev1.PodStatus{PodIP: "10.244.1.5"},
		},
	)

	got := GetClusterCIDRNoProbePod(context.Background(), cs, ns)
	set := cidrSet(got)
	for _, want := range []string{
		"10.96.0.0/12",   // component flag (service-cluster-ip-range)
		"10.96.0.0/24",   // inferred from Service ClusterIP 10.96.0.100
		"10.244.1.0/24",  // inferred from pod IP 10.244.1.5
	} {
		if !set.Has(want) {
			t.Errorf("expected %s in result, got %v", want, set.UnsortedList())
		}
	}
}
