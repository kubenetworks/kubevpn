package handler

import (
	"context"
	"net"
	"testing"
	"time"

	miekgdns "github.com/miekg/dns"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// startStubDNSServer starts a real miekg/dns UDP server on an ephemeral port
// (no root / port 53 needed) that answers using the provided handler. It returns
// the listen address (host:port) and a cleanup func.
func startStubDNSServer(t *testing.T, handler miekgdns.HandlerFunc) (addr string, cleanup func()) {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	mux := miekgdns.NewServeMux()
	mux.HandleFunc(".", handler)

	server := &miekgdns.Server{PacketConn: pc, Net: "udp", Handler: mux}
	started := make(chan struct{})
	server.NotifyStartedFunc = func() { close(started) }
	go func() { _ = server.ActivateAndServe() }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		_ = server.Shutdown()
		t.Fatal("stub DNS server did not start")
	}

	return pc.LocalAddr().String(), func() { _ = server.Shutdown() }
}

func aRecord(name string, ip string) *miekgdns.A {
	return &miekgdns.A{
		Hdr: miekgdns.RR_Header{Name: name, Rrtype: miekgdns.TypeA, Class: miekgdns.ClassINET, Ttl: 60},
		A:   net.ParseIP(ip),
	}
}

func aaaaRecord(name string, ip string) *miekgdns.AAAA {
	return &miekgdns.AAAA{
		Hdr:  miekgdns.RR_Header{Name: name, Rrtype: miekgdns.TypeAAAA, Class: miekgdns.ClassINET, Ttl: 60},
		AAAA: net.ParseIP(ip),
	}
}

func cnameRecord(name string, target string) *miekgdns.CNAME {
	return &miekgdns.CNAME{
		Hdr:    miekgdns.RR_Header{Name: name, Rrtype: miekgdns.TypeCNAME, Class: miekgdns.ClassINET, Ttl: 60},
		Target: target,
	}
}

func TestResolveOnce(t *testing.T) {
	const domain = "db.example.com"
	fqdn := miekgdns.Fqdn(domain)

	t.Run("A record resolved", func(t *testing.T) {
		addr, cleanup := startStubDNSServer(t, func(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
			m := new(miekgdns.Msg)
			m.SetReply(r)
			if r.Question[0].Qtype == miekgdns.TypeA {
				m.Answer = append(m.Answer, aRecord(fqdn, "10.0.100.1"))
			}
			_ = w.WriteMsg(m)
		})
		defer cleanup()

		ip := resolveOnce(context.Background(), addr, domain, miekgdns.TypeA)
		if ip == nil || ip.String() != "10.0.100.1" {
			t.Fatalf("expected 10.0.100.1, got %v", ip)
		}
	})

	t.Run("AAAA record resolved", func(t *testing.T) {
		addr, cleanup := startStubDNSServer(t, func(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
			m := new(miekgdns.Msg)
			m.SetReply(r)
			if r.Question[0].Qtype == miekgdns.TypeAAAA {
				m.Answer = append(m.Answer, aaaaRecord(fqdn, "fd00::1"))
			}
			_ = w.WriteMsg(m)
		})
		defer cleanup()

		ip := resolveOnce(context.Background(), addr, domain, miekgdns.TypeAAAA)
		if ip == nil || ip.String() != "fd00::1" {
			t.Fatalf("expected fd00::1, got %v", ip)
		}
	})

	t.Run("CNAME chain returns terminal A record", func(t *testing.T) {
		addr, cleanup := startStubDNSServer(t, func(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
			m := new(miekgdns.Msg)
			m.SetReply(r)
			// mimic a private-link CNAME chain followed by the terminal A
			m.Answer = append(m.Answer,
				cnameRecord(fqdn, "private.db.example.com."),
				aRecord("private.db.example.com.", "10.0.200.5"),
			)
			_ = w.WriteMsg(m)
		})
		defer cleanup()

		ip := resolveOnce(context.Background(), addr, domain, miekgdns.TypeA)
		if ip == nil || ip.String() != "10.0.200.5" {
			t.Fatalf("expected 10.0.200.5, got %v", ip)
		}
	})

	t.Run("no record returns nil", func(t *testing.T) {
		addr, cleanup := startStubDNSServer(t, func(w miekgdns.ResponseWriter, r *miekgdns.Msg) {
			m := new(miekgdns.Msg)
			m.SetReply(r)
			_ = w.WriteMsg(m)
		})
		defer cleanup()

		if ip := resolveOnce(context.Background(), addr, domain, miekgdns.TypeA); ip != nil {
			t.Fatalf("expected nil, got %v", ip)
		}
	})

	t.Run("server unreachable returns nil", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		// 127.0.0.1:1 has nothing listening -> error -> nil
		if ip := resolveOnce(ctx, "127.0.0.1:1", domain, miekgdns.TypeA); ip != nil {
			t.Fatalf("expected nil, got %v", ip)
		}
	})
}

// TestAddExtraRoute_FallbackToIngress wires AddExtraRoute against a real (fake)
// k8s clientset: cluster DNS is unreachable so resolution fails and the function
// must fall back to the ingress load-balancer IP, recording it in extraHost.
func TestAddExtraRoute_FallbackToIngress(t *testing.T) {
	clientset := fake.NewSimpleClientset(&networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: "svc.cluster.example.com"}},
		},
		Status: networkingv1.IngressStatus{
			LoadBalancer: networkingv1.IngressLoadBalancerStatus{
				Ingress: []networkingv1.IngressLoadBalancerIngress{{IP: "10.1.2.3"}},
			},
		},
	})

	nm := &NetworkManager{cfg: NetworkConfig{
		Clientset:        clientset,
		ManagerNamespace: "default",
		ExtraRouteInfo:   &ExtraRouteInfo{ExtraDomain: []string{"svc.cluster.example.com"}},
	}}
	// tunName is empty, so AddRoute is a no-op and no real TUN is required.

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Point at a nameserver that is not listening so the cluster-DNS query fails
	// fast and the ingress fallback kicks in.
	if err := nm.AddExtraRoute(ctx, []string{"127.0.0.1"}); err != nil {
		t.Fatalf("AddExtraRoute returned error: %v", err)
	}

	hosts := nm.GetExtraHost()
	if len(hosts) != 1 || hosts[0].IP != "10.1.2.3" || hosts[0].Domain != "svc.cluster.example.com" {
		t.Fatalf("expected extraHost [{10.1.2.3 svc.cluster.example.com}], got %+v", hosts)
	}
}
