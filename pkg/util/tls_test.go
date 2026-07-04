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
