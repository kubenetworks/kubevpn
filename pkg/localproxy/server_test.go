package localproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	xproxy "golang.org/x/net/proxy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type fakeConnector struct {
	addr string

	mu    sync.Mutex
	hosts []string
	ports []int
}

func (f *fakeConnector) Connect(ctx context.Context, host string, port int) (net.Conn, error) {
	f.mu.Lock()
	f.hosts = append(f.hosts, host)
	f.ports = append(f.ports, port)
	f.mu.Unlock()

	var d net.Dialer
	return d.DialContext(ctx, "tcp", f.addr)
}

func (f *fakeConnector) lastTarget() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hosts) == 0 {
		return "", 0
	}
	return f.hosts[len(f.hosts)-1], f.ports[len(f.ports)-1]
}

type fakeClusterAPI struct {
	services  map[string]*corev1.Service
	endpoints map[string]*corev1.Endpoints
	pods      []*corev1.Pod
}

func (f *fakeClusterAPI) GetService(_ context.Context, namespace, name string) (*corev1.Service, error) {
	svc, ok := f.services[namespace+"/"+name]
	if !ok {
		return nil, fmt.Errorf("service %s/%s not found", namespace, name)
	}
	return svc, nil
}

func (f *fakeClusterAPI) GetEndpoints(_ context.Context, namespace, name string) (*corev1.Endpoints, error) {
	ep, ok := f.endpoints[namespace+"/"+name]
	if !ok {
		return nil, fmt.Errorf("endpoints %s/%s not found", namespace, name)
	}
	return ep, nil
}

func (f *fakeClusterAPI) ListServices(_ context.Context) (*corev1.ServiceList, error) {
	list := &corev1.ServiceList{}
	for _, svc := range f.services {
		list.Items = append(list.Items, *svc)
	}
	return list, nil
}

func (f *fakeClusterAPI) ListPodsByIP(_ context.Context, ip string) (*corev1.PodList, error) {
	list := &corev1.PodList{}
	for _, pod := range f.pods {
		if pod.Status.PodIP == ip {
			list.Items = append(list.Items, *pod)
		}
	}
	return list, nil
}

func TestParseServiceHost(t *testing.T) {
	tests := []struct {
		host      string
		defaultNS string
		wantSvc   string
		wantNS    string
		wantOK    bool
	}{
		{host: "jupyter.utils.svc.cluster.local", defaultNS: "default", wantSvc: "jupyter", wantNS: "utils", wantOK: true},
		{host: "jupyter.utils.svc", defaultNS: "default", wantSvc: "jupyter", wantNS: "utils", wantOK: true},
		{host: "jupyter.utils", defaultNS: "default", wantSvc: "jupyter", wantNS: "utils", wantOK: true},
		{host: "jupyter", defaultNS: "utils", wantSvc: "jupyter", wantNS: "utils", wantOK: true},
		{host: "api.example.com", defaultNS: "utils", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			gotSvc, gotNS, gotOK := parseServiceHost(tt.host, tt.defaultNS)
			if gotSvc != tt.wantSvc || gotNS != tt.wantNS || gotOK != tt.wantOK {
				t.Fatalf("parseServiceHost(%q) = (%q, %q, %v), want (%q, %q, %v)", tt.host, gotSvc, gotNS, gotOK, tt.wantSvc, tt.wantNS, tt.wantOK)
			}
		})
	}
}

func TestResolveServiceToPod(t *testing.T) {
	api := &fakeClusterAPI{
		services: map[string]*corev1.Service{
			"utils/jupyter": {
				ObjectMeta: metav1.ObjectMeta{Name: "jupyter", Namespace: "utils"},
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.103.245.38",
					Ports:     []corev1.ServicePort{{Name: "http", Port: 8086}},
				},
			},
		},
		endpoints: map[string]*corev1.Endpoints{
			"utils/jupyter": {
				ObjectMeta: metav1.ObjectMeta{Name: "jupyter", Namespace: "utils"},
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{{
						IP: "10.0.1.243",
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Name:      "jupyter-0",
							Namespace: "utils",
						},
					}},
					Ports: []corev1.EndpointPort{{Name: "http", Port: 8086}},
				}},
			},
		},
	}

	connector := &ClusterConnector{
		Client:           api,
		DefaultNamespace: "default",
	}

	target, err := connector.resolveServiceHost(context.Background(), "jupyter.utils.svc.cluster.local", 8086)
	if err != nil {
		t.Fatal(err)
	}
	if target.PodName != "jupyter-0" || target.Namespace != "utils" || target.PodPort != 8086 {
		t.Fatalf("unexpected target: %#v", target)
	}
}

func TestResolveTargetByClusterIP(t *testing.T) {
	api := &fakeClusterAPI{
		services: map[string]*corev1.Service{
			"utils/jupyter": {
				ObjectMeta: metav1.ObjectMeta{Name: "jupyter", Namespace: "utils"},
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.103.245.38",
					Ports: []corev1.ServicePort{{
						Name:       "http",
						Port:       8086,
						TargetPort: intstr.FromInt(18086),
					}},
				},
			},
		},
		endpoints: map[string]*corev1.Endpoints{
			"utils/jupyter": {
				ObjectMeta: metav1.ObjectMeta{Name: "jupyter", Namespace: "utils"},
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{{
						IP: "10.0.1.243",
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Name:      "jupyter-0",
							Namespace: "utils",
						},
					}},
					Ports: []corev1.EndpointPort{{Name: "http", Port: 18086}},
				}},
			},
		},
	}

	connector := &ClusterConnector{Client: api, DefaultNamespace: "default"}
	target, err := connector.resolveTarget(context.Background(), "10.103.245.38", 8086)
	if err != nil {
		t.Fatal(err)
	}
	if target.PodName != "jupyter-0" || target.Namespace != "utils" || target.PodPort != 18086 {
		t.Fatalf("unexpected clusterIP target: %#v", target)
	}
}

func TestResolveTargetByPodIP(t *testing.T) {
	api := &fakeClusterAPI{
		pods: []*corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{Name: "jupyter-0", Namespace: "utils"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.1.243"},
		}},
	}

	connector := &ClusterConnector{Client: api, DefaultNamespace: "default"}
	target, err := connector.resolveTarget(context.Background(), "10.0.1.243", 8086)
	if err != nil {
		t.Fatal(err)
	}
	if target.PodName != "jupyter-0" || target.Namespace != "utils" || target.PodPort != 8086 {
		t.Fatalf("unexpected podIP target: %#v", target)
	}
}

func TestResolveTargetUnsupportedHost(t *testing.T) {
	connector := &ClusterConnector{Client: &fakeClusterAPI{}, DefaultNamespace: "default"}
	if _, err := connector.resolveTarget(context.Background(), "api.example.com", 443); err == nil {
		t.Fatal("expected unsupported host resolution to fail")
	}
}

func TestResolveServiceToPodNoReadyEndpoints(t *testing.T) {
	api := &fakeClusterAPI{
		services: map[string]*corev1.Service{
			"utils/jupyter": {
				ObjectMeta: metav1.ObjectMeta{Name: "jupyter", Namespace: "utils"},
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.103.245.38",
					Ports:     []corev1.ServicePort{{Name: "http", Port: 8086}},
				},
			},
		},
		endpoints: map[string]*corev1.Endpoints{
			"utils/jupyter": {
				ObjectMeta: metav1.ObjectMeta{Name: "jupyter", Namespace: "utils"},
			},
		},
	}

	connector := &ClusterConnector{Client: api, DefaultNamespace: "default"}
	if _, err := connector.resolveServiceHost(context.Background(), "jupyter.utils.svc.cluster.local", 8086); err == nil {
		t.Fatal("expected resolution to fail when service has no ready endpoints")
	}
}

func TestSOCKS5ProxyPreservesHostname(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tree/notebooks" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()

	connector := &fakeConnector{addr: strings.TrimPrefix(backend.URL, "http://")}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		_ = ServeSOCKS5(ctx, ln, connector)
	}()

	dialer, err := xproxy.SOCKS5("tcp", ln.Addr().String(), nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		},
	}

	resp, err := httpClient.Get("http://jupyter.utils:8086/tree/notebooks")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" {
		t.Fatalf("unexpected body %q", string(body))
	}
	host, port := connector.lastTarget()
	if host != "jupyter.utils" || port != 8086 {
		t.Fatalf("proxy connected to %s:%d, want jupyter.utils:8086", host, port)
	}
}

func TestHTTPConnectProxyPreservesHostname(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tree/notebooks" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()

	connector := &fakeConnector{addr: strings.TrimPrefix(backend.URL, "http://")}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		_ = ServeHTTPConnect(ctx, ln, connector)
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_, err = fmt.Fprintf(conn, "CONNECT jupyter.utils:8086 HTTP/1.1\r\nHost: jupyter.utils:8086\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected connect status %s", resp.Status)
	}

	_, err = fmt.Fprintf(conn, "GET /tree/notebooks HTTP/1.1\r\nHost: jupyter.utils:8086\r\nConnection: close\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	httpResp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" {
		t.Fatalf("unexpected body %q", string(body))
	}
	host, port := connector.lastTarget()
	if host != "jupyter.utils" || port != 8086 {
		t.Fatalf("proxy connected to %s:%d, want jupyter.utils:8086", host, port)
	}
}

func TestProxyOutE2ECase(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in short mode")
	}
	if strings.TrimSpace(getenv("KUBEVPN_PROXY_OUT_E2E", "")) == "" {
		t.Skip("set KUBEVPN_PROXY_OUT_E2E=1 to run real cluster proxy-out test")
	}

	host := getenv("KUBEVPN_PROXY_OUT_E2E_HOST", "jupyter.utils")
	path := getenv("KUBEVPN_PROXY_OUT_E2E_PATH", "/tree/notebooks")
	port := getenv("KUBEVPN_PROXY_OUT_E2E_PORT", "8086")

	config, err := restConfigFromDefault()
	if err != nil {
		t.Fatal(err)
	}
	clusterAPI, clientset, err := NewClusterAPI(config)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connector := &ClusterConnector{
		Client:           clusterAPI,
		Forwarder:        NewPodDialer(config, clientset),
		RESTConfig:       config,
		DefaultNamespace: "default",
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		_ = ServeSOCKS5(ctx, ln, connector)
	}()

	dialer, err := xproxy.SOCKS5("tcp", ln.Addr().String(), nil, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	targetURL := (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: path}).String()
	resp, err := httpClient.Get(targetURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unexpected status %s body=%s", resp.Status, string(body))
	}
}
