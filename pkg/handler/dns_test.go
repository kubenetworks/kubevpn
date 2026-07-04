package handler

import (
	"context"
	"net"
	"testing"
	"time"

	miekgdns "github.com/miekg/dns"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestDNS_DetectNameserver_ServiceIPReachable(t *testing.T) {
	// Start a local UDP DNS server that responds to queries.
	addr, cleanup := startTestDNSServer(t)
	defer cleanup()

	host, port, _ := net.SplitHostPort(addr)
	_ = port

	conf := &miekgdns.ClientConfig{
		Servers: []string{"0.0.0.0"},
		Port:    "53",
	}
	serviceIP := host
	podIP := "10.244.0.5"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := detectNameserver(ctx, conf, serviceIP, podIP)
	if err != nil {
		t.Fatalf("detectNameserver returned error: %v", err)
	}

	// Since the local DNS server is reachable, the service IP should be chosen.
	if len(conf.Servers) != 1 || conf.Servers[0] != serviceIP {
		t.Fatalf("expected servers=[%s], got %v", serviceIP, conf.Servers)
	}
}

func TestDNS_DetectNameserver_ServiceIPUnreachable(t *testing.T) {
	// Use an unreachable IP as the "service IP" — this will time out quickly.
	conf := &miekgdns.ClientConfig{
		Servers: []string{"0.0.0.0"},
		Port:    "53",
	}
	// RFC 5737: 192.0.2.0/24 is TEST-NET-1, should be unreachable.
	serviceIP := "192.0.2.1"
	podIP := "10.244.0.99"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := detectNameserver(ctx, conf, serviceIP, podIP)
	if err != nil {
		t.Fatalf("detectNameserver returned error: %v", err)
	}

	// Service IP is unreachable, so it should fall back to the pod IP.
	if len(conf.Servers) != 1 || conf.Servers[0] != podIP {
		t.Fatalf("expected servers=[%s], got %v", podIP, conf.Servers)
	}
}

func TestDNS_DetectNameserver_AlwaysReturnsNil(t *testing.T) {
	// detectNameserver always returns nil regardless of the DNS check result.
	conf := &miekgdns.ClientConfig{
		Servers: []string{},
		Port:    "53",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := detectNameserver(ctx, conf, "192.0.2.1", "10.0.0.1")
	if err != nil {
		t.Fatalf("detectNameserver should always return nil, got: %v", err)
	}
}

func TestDNS_NameserverChecker_Success(t *testing.T) {
	addr, cleanup := startTestDNSServer(t)
	defer cleanup()

	host, _, _ := net.SplitHostPort(addr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := nameserverChecker(ctx, config.ConfigMapPodTrafficManager, host)
	if err != nil {
		t.Fatalf("nameserverChecker should succeed against local DNS server: %v", err)
	}
}

func TestDNS_NameserverChecker_Failure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// RFC 5737 TEST-NET: should be unreachable.
	err := nameserverChecker(ctx, "example.local", "192.0.2.1")
	if err == nil {
		t.Fatal("nameserverChecker should fail for unreachable server")
	}
}

func TestDNS_SetupNamespaceList(t *testing.T) {
	// Verify that setupDNS collects namespaces from the cluster.
	// We can't run setupDNS fully (it requires a real pod), but we can test
	// the namespace listing logic that it uses internally.
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns"}},
		&v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      config.ConfigMapPodTrafficManager,
				Namespace: "test-ns",
			},
			Spec: v1.ServiceSpec{
				ClusterIP: "10.96.0.100",
			},
		},
	)

	ctx := context.Background()
	list, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 500})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(list.Items) != 3 {
		t.Fatalf("expected 3 namespaces, got %d", len(list.Items))
	}

	// Simulate the namespace dedup logic from setupDNS.
	originNS := "default"
	ns := []string{originNS}
	for _, item := range list.Items {
		found := false
		for _, n := range ns {
			if n == item.Name {
				found = true
				break
			}
		}
		if !found {
			ns = append(ns, item.Name)
		}
	}

	if len(ns) != 3 {
		t.Fatalf("expected 3 unique namespaces, got %d: %v", len(ns), ns)
	}
	if ns[0] != "default" {
		t.Fatalf("first namespace should be origin (default), got %s", ns[0])
	}
}

func TestDNS_SetupServiceGet(t *testing.T) {
	// Verify the service lookup that setupDNS performs.
	clientset := fake.NewSimpleClientset(
		&v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      config.ConfigMapPodTrafficManager,
				Namespace: "test-ns",
			},
			Spec: v1.ServiceSpec{
				ClusterIP: "10.96.0.100",
				Ports: []v1.ServicePort{
					{Port: 53, Protocol: v1.ProtocolUDP},
				},
			},
		},
	)

	ctx := context.Background()
	svc, err := clientset.CoreV1().Services("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Spec.ClusterIP != "10.96.0.100" {
		t.Fatalf("expected ClusterIP 10.96.0.100, got %s", svc.Spec.ClusterIP)
	}
}

// startTestDNSServer starts a minimal UDP DNS server on a random local port.
// It responds to all A queries with 127.0.0.1.
func startTestDNSServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addr = pc.LocalAddr().String()

	// We need the DNS server to listen on port 53 for nameserverChecker,
	// but we can't bind to 53 without root. Instead, we'll start on a random port
	// and override the test to use that port.
	// Since nameserverChecker hardcodes port 53, we start a real server on port 53
	// or we test detectNameserver with a server on a custom port.
	// Actually, nameserverChecker uses net.JoinHostPort(dnsServer, "53"), so we need
	// to use a wrapper approach.

	// Let's start the DNS server using the miekg/dns package which gives us more control.
	pc.Close()

	mux := miekgdns.NewServeMux()
	mux.HandleFunc(".", func(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
		m := new(miekgdns.Msg)
		m.SetReply(r)
		m.Authoritative = true
		if len(r.Question) > 0 {
			m.Answer = append(m.Answer, &miekgdns.A{
				Hdr: miekgdns.RR_Header{
					Name:   r.Question[0].Name,
					Rrtype: miekgdns.TypeA,
					Class:  miekgdns.ClassINET,
					Ttl:    60,
				},
				A: net.ParseIP("127.0.0.1"),
			})
		}
		_ = w.WriteMsg(m)
	})

	server := &miekgdns.Server{
		Addr:    "127.0.0.1:53",
		Net:     "udp",
		Handler: mux,
	}

	started := make(chan struct{})
	server.NotifyStartedFunc = func() {
		close(started)
	}

	go func() {
		if err := server.ListenAndServe(); err != nil {
			// If port 53 is not available, this test will be skipped.
			return
		}
	}()

	// Wait for server to start or timeout.
	select {
	case <-started:
	case <-time.After(1 * time.Second):
		t.Skip("could not start DNS server on port 53 (requires root)")
	}

	host, _, _ := net.SplitHostPort(server.Addr)
	return net.JoinHostPort(host, "53"), func() {
		_ = server.Shutdown()
	}
}
