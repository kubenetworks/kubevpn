package util

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestTLS(t *testing.T) {
	ns := "kubevpn"
	crtBytes, keyBytes, alternateDNS, err := GenTLSCert(context.Background(), GetTLSHost(ns))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.TLSCertKey, string(crtBytes))
	t.Setenv(config.TLSPrivateKeyKey, string(keyBytes))
	t.Setenv(config.TLSServerName, string(alternateDNS))

	listen, err := net.Listen("tcp", ":9090")
	if err != nil {
		t.Fatal(err)
	}
	tlsServerConfig, err := GetTlsServerConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	listener := tls.NewListener(listen, tlsServerConfig)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				t.Error(err)
				return
			}
			go func(conn net.Conn) {
				bytes := make([]byte, 1024)
				all, err2 := conn.Read(bytes)
				if err2 != nil {
					t.Error(err2)
					return
				}
				defer conn.Close()
				t.Log(string(bytes[:all]))
				io.WriteString(conn, "hello client")
			}(conn)
		}
	}()
	dial, err := net.Dial("tcp", ":9090")
	if err != nil {
		t.Fatal(err)
	}

	clientConfig, err := GetTlsClientConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := tls.Client(dial, clientConfig)
	client.Write([]byte("hi server"))
	all, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(all))
}

func TestGetTlsServerConfig_NilInput(t *testing.T) {
	// Ensure no TLS env vars are set so the fallback path returns ErrNoTLSConfig.
	t.Setenv(config.TLSCertKey, "")
	t.Setenv(config.TLSPrivateKeyKey, "")
	t.Setenv(config.TLSServerName, "")

	_, err := GetTlsServerConfig(nil)
	if err == nil {
		t.Fatal("expected error for nil input with no env vars, got nil")
	}
	if !errors.Is(err, ErrNoTLSConfig) {
		t.Fatalf("expected ErrNoTLSConfig, got: %v", err)
	}
}

func TestGenTLSCert(t *testing.T) {
	ns := "test-namespace"
	crt, key, serverName, err := GenTLSCert(context.Background(), ns)
	if err != nil {
		t.Fatalf("GenTLSCert: %v", err)
	}
	if len(crt) == 0 {
		t.Fatal("GenTLSCert returned empty certificate")
	}
	if len(key) == 0 {
		t.Fatal("GenTLSCert returned empty key")
	}
	if len(serverName) == 0 {
		t.Fatal("GenTLSCert returned empty server name")
	}
	expectedHost := config.ConfigMapPodTrafficManager + "." + ns
	if string(serverName) != expectedHost {
		t.Fatalf("server name: want %q, got %q", expectedHost, string(serverName))
	}

	// Verify the cert and key form a valid TLS key pair.
	_, err = tls.X509KeyPair(crt, key)
	if err != nil {
		t.Fatalf("generated cert/key is not a valid X509 key pair: %v", err)
	}
}

func TestServerName(t *testing.T) {
	cases := []struct {
		namespace string
		want      string
	}{
		{"default", config.ConfigMapPodTrafficManager + ".default"},
		{"kube-system", config.ConfigMapPodTrafficManager + ".kube-system"},
		{"my-ns", config.ConfigMapPodTrafficManager + ".my-ns"},
	}
	for _, c := range cases {
		t.Run(c.namespace, func(t *testing.T) {
			got := GetTLSHost(c.namespace)
			if got != c.want {
				t.Fatalf("GetTLSHost(%q): want %q, got %q", c.namespace, c.want, got)
			}
		})
	}
}

func TestIpsToStrings(t *testing.T) {
	cases := []struct {
		name string
		ips  []net.IP
		want []string
	}{
		{"nil input", nil, []string{}},
		{"empty slice", []net.IP{}, []string{}},
		{"single IPv4", []net.IP{net.IPv4(127, 0, 0, 1)}, []string{"127.0.0.1"}},
		{"multiple IPv4", []net.IP{net.IPv4(10, 0, 0, 1), net.IPv4(192, 168, 1, 1)}, []string{"10.0.0.1", "192.168.1.1"}},
		{"IPv6 loopback", []net.IP{net.IPv6loopback}, []string{"::1"}},
		{"mixed", []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, []string{"127.0.0.1", "::1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ipsToStrings(c.ips)
			if len(got) != len(c.want) {
				t.Fatalf("got %d elements, want %d", len(got), len(c.want))
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("[%d]: got %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestGetTlsClientConfig_WithValidCert(t *testing.T) {
	crt, key, serverName, err := GenTLSCert(context.Background(), "test-ns")
	if err != nil {
		t.Fatalf("GenTLSCert: %v", err)
	}
	secret := map[string][]byte{
		config.TLSCertKey:       crt,
		config.TLSPrivateKeyKey: key,
		config.TLSServerName:    serverName,
	}
	cfg, err := GetTlsClientConfig(secret)
	if err != nil {
		t.Fatalf("GetTlsClientConfig: %v", err)
	}
	if cfg.ServerName != string(serverName) {
		t.Errorf("ServerName: got %q, want %q", cfg.ServerName, string(serverName))
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion: got %d, want %d", cfg.MinVersion, tls.VersionTLS13)
	}
	if cfg.RootCAs == nil {
		t.Error("RootCAs should not be nil")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates: got %d, want 1", len(cfg.Certificates))
	}
}

func TestGetTlsClientConfig_InvalidKeyPair(t *testing.T) {
	secret := map[string][]byte{
		config.TLSCertKey:       []byte("not-a-cert"),
		config.TLSPrivateKeyKey: []byte("not-a-key"),
		config.TLSServerName:    []byte("test"),
	}
	_, err := GetTlsClientConfig(secret)
	if err == nil {
		t.Fatal("expected error for invalid cert/key pair")
	}
}

func TestGetTlsClientConfig_NilInput(t *testing.T) {
	t.Setenv(config.TLSCertKey, "")
	t.Setenv(config.TLSPrivateKeyKey, "")
	t.Setenv(config.TLSServerName, "")
	_, err := GetTlsClientConfig(nil)
	if !errors.Is(err, ErrNoTLSConfig) {
		t.Fatalf("expected ErrNoTLSConfig, got: %v", err)
	}
}

func TestGetTlsServerConfig_WithValidCert(t *testing.T) {
	crt, key, serverName, err := GenTLSCert(context.Background(), "test-ns")
	if err != nil {
		t.Fatalf("GenTLSCert: %v", err)
	}
	secret := map[string][]byte{
		config.TLSCertKey:       crt,
		config.TLSPrivateKeyKey: key,
		config.TLSServerName:    serverName,
	}
	cfg, err := GetTlsServerConfig(secret)
	if err != nil {
		t.Fatalf("GetTlsServerConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion: got %d, want %d", cfg.MinVersion, tls.VersionTLS13)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates: got %d, want 1", len(cfg.Certificates))
	}
}

func TestGetTlsServerConfig_InvalidKeyPair(t *testing.T) {
	secret := map[string][]byte{
		config.TLSCertKey:       []byte("not-a-cert"),
		config.TLSPrivateKeyKey: []byte("not-a-key"),
		config.TLSServerName:    []byte("test"),
	}
	_, err := GetTlsServerConfig(secret)
	if err == nil {
		t.Fatal("expected error for invalid cert/key pair")
	}
}
