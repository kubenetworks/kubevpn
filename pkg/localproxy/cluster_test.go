package localproxy

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestEndpointPortMatches(t *testing.T) {
	tests := []struct {
		name          string
		servicePort   *corev1.ServicePort
		endpointPort  *corev1.EndpointPort
		requestedPort int
		want          bool
	}{
		{
			name:          "nil service port",
			servicePort:   nil,
			endpointPort:  &corev1.EndpointPort{Port: 80},
			requestedPort: 80,
			want:          false,
		},
		{
			name:          "nil endpoint port",
			servicePort:   &corev1.ServicePort{Port: 80},
			endpointPort:  nil,
			requestedPort: 80,
			want:          false,
		},
		{
			name:          "both nil",
			servicePort:   nil,
			endpointPort:  nil,
			requestedPort: 80,
			want:          false,
		},
		{
			name:          "matching named ports",
			servicePort:   &corev1.ServicePort{Name: "http", Port: 80},
			endpointPort:  &corev1.EndpointPort{Name: "http", Port: 8080},
			requestedPort: 80,
			want:          true,
		},
		{
			name:          "different named ports",
			servicePort:   &corev1.ServicePort{Name: "http", Port: 80},
			endpointPort:  &corev1.EndpointPort{Name: "grpc", Port: 8080},
			requestedPort: 80,
			want:          false,
		},
		{
			name:          "endpoint port matches requested port",
			servicePort:   &corev1.ServicePort{Port: 80},
			endpointPort:  &corev1.EndpointPort{Port: 80},
			requestedPort: 80,
			want:          true,
		},
		{
			name:          "endpoint port does not match requested port",
			servicePort:   &corev1.ServicePort{Port: 80},
			endpointPort:  &corev1.EndpointPort{Port: 9090},
			requestedPort: 80,
			want:          false,
		},
		{
			name:          "target port matches endpoint port",
			servicePort:   &corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt(8080)},
			endpointPort:  &corev1.EndpointPort{Port: 8080},
			requestedPort: 80,
			want:          true,
		},
		{
			name:          "target port does not match endpoint port",
			servicePort:   &corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt(8080)},
			endpointPort:  &corev1.EndpointPort{Port: 9090},
			requestedPort: 80,
			want:          false,
		},
		{
			name:          "only service port has name",
			servicePort:   &corev1.ServicePort{Name: "http", Port: 80},
			endpointPort:  &corev1.EndpointPort{Port: 80},
			requestedPort: 80,
			want:          true,
		},
		{
			name:          "only endpoint port has name",
			servicePort:   &corev1.ServicePort{Port: 80},
			endpointPort:  &corev1.EndpointPort{Name: "http", Port: 80},
			requestedPort: 80,
			want:          true,
		},
		{
			name:          "string target port zero int value",
			servicePort:   &corev1.ServicePort{Port: 80, TargetPort: intstr.FromString("http")},
			endpointPort:  &corev1.EndpointPort{Port: 9090},
			requestedPort: 80,
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := endpointPortMatches(tt.servicePort, tt.endpointPort, tt.requestedPort)
			if got != tt.want {
				t.Fatalf("endpointPortMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseServiceHostTrailingDot(t *testing.T) {
	svc, ns, ok := parseServiceHost("jupyter.utils.svc.cluster.local.", "default")
	if !ok {
		t.Fatal("expected ok=true for trailing-dot FQDN")
	}
	if svc != "jupyter" || ns != "utils" {
		t.Fatalf("got (%q, %q), want (jupyter, utils)", svc, ns)
	}
}

func TestParseServiceHostEmptyDefaultNamespace(t *testing.T) {
	_, _, ok := parseServiceHost("jupyter", "")
	if ok {
		t.Fatal("expected ok=false for single-part host with empty default namespace")
	}
}

func TestParseServiceHostFourPartsWithSvc(t *testing.T) {
	// 4-part host with "svc" at index 2 matches the >= 3 && parts[2]=="svc" rule
	svc, ns, ok := parseServiceHost("jupyter.utils.svc.extra", "default")
	if !ok {
		t.Fatal("expected ok=true for 4-part host with svc at index 2")
	}
	if svc != "jupyter" || ns != "utils" {
		t.Fatalf("got (%q, %q), want (jupyter, utils)", svc, ns)
	}
}

func TestParseServiceHostFourPartsWithoutSvc(t *testing.T) {
	// 4-part host without "svc" at any recognized position should fail
	_, _, ok := parseServiceHost("jupyter.utils.data.extra", "default")
	if ok {
		t.Fatal("expected ok=false for 4-part host without recognized pattern")
	}
}

func TestResolveServiceToPodNilService(t *testing.T) {
	connector := &ClusterConnector{Client: &fakeClusterAPI{}, DefaultNamespace: "default"}
	_, err := connector.resolveServiceToPod(context.Background(), nil, 80)
	if err == nil {
		t.Fatal("expected error for nil service")
	}
}

func TestResolveServiceToPodSinglePortFallback(t *testing.T) {
	api := &fakeClusterAPI{
		services: map[string]*corev1.Service{
			"default/web": {
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{Name: "http", Port: 8080}},
				},
			},
		},
		endpoints: map[string]*corev1.Endpoints{
			"default/web": {
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{{
						IP: "10.0.0.5",
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Name:      "web-abc",
							Namespace: "default",
						},
					}},
					Ports: []corev1.EndpointPort{{Name: "http", Port: 8080}},
				}},
			},
		},
	}

	connector := &ClusterConnector{Client: api, DefaultNamespace: "default"}
	svc := api.services["default/web"]

	target, err := connector.resolveServiceToPod(context.Background(), svc, 9999)
	if err != nil {
		t.Fatalf("expected single-port fallback, got error: %v", err)
	}
	if target.PodName != "web-abc" || target.PodPort != 8080 {
		t.Fatalf("unexpected target: %#v", target)
	}
}

func TestResolveServiceToPodMultiPortNoMatch(t *testing.T) {
	api := &fakeClusterAPI{
		services: map[string]*corev1.Service{
			"default/web": {
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Port: 8080},
						{Name: "https", Port: 8443},
					},
				},
			},
		},
		endpoints: map[string]*corev1.Endpoints{
			"default/web": {
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{{
						IP: "10.0.0.5",
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Name:      "web-abc",
							Namespace: "default",
						},
					}},
					Ports: []corev1.EndpointPort{
						{Name: "http", Port: 8080},
						{Name: "https", Port: 8443},
					},
				}},
			},
		},
	}

	connector := &ClusterConnector{Client: api, DefaultNamespace: "default"}
	svc := api.services["default/web"]

	_, err := connector.resolveServiceToPod(context.Background(), svc, 9999)
	if err == nil {
		t.Fatal("expected error when no port matches on multi-port service")
	}
}

func TestResolveServiceToPodEndpointWithoutTargetRef(t *testing.T) {
	api := &fakeClusterAPI{
		services: map[string]*corev1.Service{
			"default/external": {
				ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "default"},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{Name: "http", Port: 80}},
				},
			},
		},
		endpoints: map[string]*corev1.Endpoints{
			"default/external": {
				ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "default"},
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{{
						IP: "10.0.0.99",
					}},
					Ports: []corev1.EndpointPort{{Name: "http", Port: 80}},
				}},
			},
		},
		pods: []*corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{Name: "external-pod-0", Namespace: "staging"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.99"},
		}},
	}

	connector := &ClusterConnector{Client: api, DefaultNamespace: "default"}
	svc := api.services["default/external"]

	target, err := connector.resolveServiceToPod(context.Background(), svc, 80)
	if err != nil {
		t.Fatalf("expected fallback to pod IP lookup, got error: %v", err)
	}
	if target.PodName != "external-pod-0" || target.Namespace != "staging" {
		t.Fatalf("unexpected target: %#v", target)
	}
	if target.PodPort != 80 {
		t.Fatalf("expected PodPort=80, got %d", target.PodPort)
	}
}

func TestResolveServiceToPodSubsetSinglePortFallback(t *testing.T) {
	api := &fakeClusterAPI{
		services: map[string]*corev1.Service{
			"default/svc": {
				ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{Name: "web", Port: 80}},
				},
			},
		},
		endpoints: map[string]*corev1.Endpoints{
			"default/svc": {
				ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{{
						IP: "10.0.0.10",
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Name:      "svc-0",
							Namespace: "default",
						},
					}},
					Ports: []corev1.EndpointPort{{Name: "app", Port: 3000}},
				}},
			},
		},
	}

	connector := &ClusterConnector{Client: api, DefaultNamespace: "default"}
	svc := api.services["default/svc"]

	target, err := connector.resolveServiceToPod(context.Background(), svc, 80)
	if err != nil {
		t.Fatalf("expected single-port subset fallback, got error: %v", err)
	}
	if target.PodPort != 3000 {
		t.Fatalf("expected PodPort=3000 from subset fallback, got %d", target.PodPort)
	}
}

func TestResolvePodIPSkipsNonRunning(t *testing.T) {
	now := metav1.Now()
	api := &fakeClusterAPI{
		pods: []*corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-pending", Namespace: "default"},
				Status:     corev1.PodStatus{Phase: corev1.PodPending, PodIP: "10.0.0.1"},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "pod-deleting",
					Namespace:         "default",
					DeletionTimestamp: &now,
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1"},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-running", Namespace: "default"},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1"},
			},
		},
	}

	connector := &ClusterConnector{Client: api, DefaultNamespace: "default"}
	target, err := connector.resolvePodIP(context.Background(), "10.0.0.1", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if target.PodName != "pod-running" {
		t.Fatalf("expected pod-running, got %q", target.PodName)
	}
}

func TestResolvePodIPNoRunningPod(t *testing.T) {
	api := &fakeClusterAPI{
		pods: []*corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pod-failed", Namespace: "default"},
				Status:     corev1.PodStatus{Phase: corev1.PodFailed, PodIP: "10.0.0.1"},
			},
		},
	}

	connector := &ClusterConnector{Client: api, DefaultNamespace: "default"}
	_, err := connector.resolvePodIP(context.Background(), "10.0.0.1", 8080)
	if err == nil {
		t.Fatal("expected error when no running pod found")
	}
}

func TestResolveServiceIPNoMatch(t *testing.T) {
	api := &fakeClusterAPI{
		services: map[string]*corev1.Service{
			"default/web": {
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.96.0.10",
					Ports:     []corev1.ServicePort{{Port: 80}},
				},
			},
		},
	}

	connector := &ClusterConnector{Client: api, DefaultNamespace: "default"}
	_, err := connector.resolveServiceIP(context.Background(), "10.96.0.99", 80)
	if err == nil {
		t.Fatal("expected error when no service matches cluster IP")
	}
}

func TestConnectNilForwarder(t *testing.T) {
	api := &fakeClusterAPI{
		services: map[string]*corev1.Service{
			"default/web": {
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{Port: 80}},
				},
			},
		},
		endpoints: map[string]*corev1.Endpoints{
			"default/web": {
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{{
						IP: "10.0.0.5",
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Name:      "web-0",
							Namespace: "default",
						},
					}},
					Ports: []corev1.EndpointPort{{Port: 80}},
				}},
			},
		},
	}

	connector := &ClusterConnector{
		Client:           api,
		Forwarder:        nil,
		DefaultNamespace: "default",
	}
	_, err := connector.Connect(context.Background(), "web", 80)
	if err == nil {
		t.Fatal("expected error for nil forwarder")
	}
	if err.Error() != "pod dialer is required" {
		t.Fatalf("unexpected error: %v", err)
	}
}

type fakePodDialer struct {
	namespace string
	podName   string
	port      int32
	err       error
}

func (f *fakePodDialer) DialPod(_ context.Context, namespace, podName string, port int32) (net.Conn, error) {
	f.namespace = namespace
	f.podName = podName
	f.port = port
	if f.err != nil {
		return nil, f.err
	}
	client, _ := net.Pipe()
	return client, nil
}

func TestConnectWithForwarder(t *testing.T) {
	api := &fakeClusterAPI{
		services: map[string]*corev1.Service{
			"default/web": {
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{Port: 80}},
				},
			},
		},
		endpoints: map[string]*corev1.Endpoints{
			"default/web": {
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{{
						IP: "10.0.0.5",
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Name:      "web-0",
							Namespace: "default",
						},
					}},
					Ports: []corev1.EndpointPort{{Port: 80}},
				}},
			},
		},
	}

	dialer := &fakePodDialer{}
	connector := &ClusterConnector{
		Client:           api,
		Forwarder:        dialer,
		DefaultNamespace: "default",
	}

	conn, err := connector.Connect(context.Background(), "web", 80)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if dialer.podName != "web-0" {
		t.Fatalf("expected pod web-0, got %q", dialer.podName)
	}
	if dialer.namespace != "default" {
		t.Fatalf("expected namespace default, got %q", dialer.namespace)
	}
	if dialer.port != 80 {
		t.Fatalf("expected port 80, got %d", dialer.port)
	}
}

func TestConnectForwarderError(t *testing.T) {
	api := &fakeClusterAPI{
		services: map[string]*corev1.Service{
			"default/web": {
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{Port: 80}},
				},
			},
		},
		endpoints: map[string]*corev1.Endpoints{
			"default/web": {
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{{
						IP: "10.0.0.5",
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Name:      "web-0",
							Namespace: "default",
						},
					}},
					Ports: []corev1.EndpointPort{{Port: 80}},
				}},
			},
		},
	}

	dialer := &fakePodDialer{err: fmt.Errorf("connection refused")}
	connector := &ClusterConnector{
		Client:           api,
		Forwarder:        dialer,
		DefaultNamespace: "default",
	}

	_, err := connector.Connect(context.Background(), "web", 80)
	if err == nil {
		t.Fatal("expected error from forwarder")
	}
}

func TestResolveTargetIPFallsBackToPod(t *testing.T) {
	api := &fakeClusterAPI{
		services: map[string]*corev1.Service{},
		pods: []*corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{Name: "standalone-pod", Namespace: "apps"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.0.5"},
		}},
	}

	connector := &ClusterConnector{Client: api, DefaultNamespace: "default"}
	target, err := connector.resolveTarget(context.Background(), "10.244.0.5", 3000)
	if err != nil {
		t.Fatal(err)
	}
	if target.PodName != "standalone-pod" || target.Namespace != "apps" || target.PodPort != 3000 {
		t.Fatalf("unexpected target: %#v", target)
	}
}

func TestIsListenerClosedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "unrelated error",
			err:  fmt.Errorf("something else"),
			want: false,
		},
		{
			name: "raw closed message",
			err:  fmt.Errorf("use of closed network connection"),
			want: true,
		},
		{
			name: "OpError wrapping closed message",
			err: &net.OpError{
				Op:  "accept",
				Net: "tcp",
				Err: fmt.Errorf("use of closed network connection"),
			},
			want: true,
		},
		{
			name: "OpError with different message",
			err: &net.OpError{
				Op:  "accept",
				Net: "tcp",
				Err: fmt.Errorf("connection reset"),
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isListenerClosedError(tt.err)
			if got != tt.want {
				t.Fatalf("isListenerClosedError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestServerListenAndServeNoConnector(t *testing.T) {
	s := &Server{}
	err := s.ListenAndServe(context.Background())
	if err == nil {
		t.Fatal("expected error for nil connector")
	}
}

func TestServerListenAndServeContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	connector := &fakeConnector{addr: "127.0.0.1:1"}

	s := &Server{
		Connector:       connector,
		SOCKSListenAddr: "127.0.0.1:0",
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.ListenAndServe(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("expected nil error on context cancel, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down after context cancel")
	}
}

func TestResolveServiceToPodNamespaceFromService(t *testing.T) {
	api := &fakeClusterAPI{
		services: map[string]*corev1.Service{
			"production/api": {
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "production"},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{Port: 443}},
				},
			},
		},
		endpoints: map[string]*corev1.Endpoints{
			"production/api": {
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "production"},
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{{
						IP: "10.0.0.20",
						TargetRef: &corev1.ObjectReference{
							Kind: "Pod",
							Name: "api-pod-xyz",
						},
					}},
					Ports: []corev1.EndpointPort{{Port: 8443}},
				}},
			},
		},
	}

	connector := &ClusterConnector{Client: api, DefaultNamespace: "default"}
	svc := api.services["production/api"]

	target, err := connector.resolveServiceToPod(context.Background(), svc, 443)
	if err != nil {
		t.Fatal(err)
	}
	if target.Namespace != "production" {
		t.Fatalf("expected namespace 'production', got %q", target.Namespace)
	}
	if target.PodName != "api-pod-xyz" {
		t.Fatalf("expected pod 'api-pod-xyz', got %q", target.PodName)
	}
}

func TestResolveServiceToPodMultipleSubsets(t *testing.T) {
	api := &fakeClusterAPI{
		services: map[string]*corev1.Service{
			"default/multi": {
				ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "default"},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{Name: "http", Port: 80}},
				},
			},
		},
		endpoints: map[string]*corev1.Endpoints{
			"default/multi": {
				ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "default"},
				Subsets: []corev1.EndpointSubset{
					{
						Addresses: []corev1.EndpointAddress{{
							IP: "10.0.0.1",
							TargetRef: &corev1.ObjectReference{
								Kind:      "Pod",
								Name:      "multi-a",
								Namespace: "default",
							},
						}},
						Ports: []corev1.EndpointPort{{Name: "http", Port: 8080}},
					},
					{
						Addresses: []corev1.EndpointAddress{{
							IP: "10.0.0.2",
							TargetRef: &corev1.ObjectReference{
								Kind:      "Pod",
								Name:      "multi-b",
								Namespace: "default",
							},
						}},
						Ports: []corev1.EndpointPort{{Name: "http", Port: 8080}},
					},
				},
			},
		},
	}

	connector := &ClusterConnector{Client: api, DefaultNamespace: "default"}
	svc := api.services["default/multi"]

	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		target, err := connector.resolveServiceToPod(context.Background(), svc, 80)
		if err != nil {
			t.Fatal(err)
		}
		seen[target.PodName] = true
	}

	if !seen["multi-a"] && !seen["multi-b"] {
		t.Fatalf("expected at least one of multi-a or multi-b, got %v", seen)
	}
}

func TestFirstNonLoopbackIPv4(t *testing.T) {
	result := FirstNonLoopbackIPv4()
	if result == "" {
		t.Skip("no non-loopback IPv4 address available")
	}
	ip := net.ParseIP(result)
	if ip == nil {
		t.Fatalf("returned invalid IP: %q", result)
	}
	if ip.To4() == nil {
		t.Fatalf("returned non-IPv4 address: %q", result)
	}
	if ip.IsLoopback() {
		t.Fatalf("returned loopback address: %q", result)
	}
}
