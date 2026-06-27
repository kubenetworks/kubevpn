package regctl

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/regclient/regclient/config"
)

func TestParseImageDSN(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantRef  string
		wantUser string
		wantPass string
	}{
		{
			name:    "plain reference",
			raw:     "registry.example.com/repo:tag",
			wantRef: "registry.example.com/repo:tag",
		},
		{
			name:    "docker hub short",
			raw:     "nginx:latest",
			wantRef: "nginx:latest",
		},
		{
			name:     "user and password",
			raw:      "admin:secret@registry.example.com/namespace/repo:v1",
			wantRef:  "registry.example.com/namespace/repo:v1",
			wantUser: "admin",
			wantPass: "secret",
		},
		{
			name:     "username only",
			raw:      "admin@registry.example.com/repo:latest",
			wantRef:  "registry.example.com/repo:latest",
			wantUser: "admin",
		},
		{
			name:    "digest reference without creds",
			raw:     "registry.example.com/repo@sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
			wantRef: "registry.example.com/repo@sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		},
		{
			name:     "creds with digest reference",
			raw:      "user:pass@registry.example.com/repo@sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
			wantRef:  "registry.example.com/repo@sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
			wantUser: "user",
			wantPass: "pass",
		},
		{
			name:     "registry with port",
			raw:      "admin:secret@registry.example.com:5000/repo:tag",
			wantRef:  "registry.example.com:5000/repo:tag",
			wantUser: "admin",
			wantPass: "secret",
		},
		{
			name:    "no at sign",
			raw:     "ghcr.io/kubenetworks/kubevpn:latest",
			wantRef: "ghcr.io/kubenetworks/kubevpn:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRef, gotUser, gotPass := parseImageDSN(tt.raw)
			if gotRef != tt.wantRef {
				t.Errorf("imageRef = %q, want %q", gotRef, tt.wantRef)
			}
			if gotUser != tt.wantUser {
				t.Errorf("user = %q, want %q", gotUser, tt.wantUser)
			}
			if gotPass != tt.wantPass {
				t.Errorf("pass = %q, want %q", gotPass, tt.wantPass)
			}
		})
	}
}

func TestExtractRegistry(t *testing.T) {
	tests := []struct {
		imageRef string
		want     string
	}{
		{"registry.example.com/repo:tag", "registry.example.com"},
		{"registry.example.com:5000/repo:tag", "registry.example.com:5000"},
		{"ghcr.io/kubenetworks/kubevpn:latest", "ghcr.io"},
		{"nginx:latest", "docker.io"},
	}
	for _, tt := range tests {
		t.Run(tt.imageRef, func(t *testing.T) {
			got := extractRegistry(tt.imageRef)
			if got != tt.want {
				t.Errorf("extractRegistry(%q) = %q, want %q", tt.imageRef, got, tt.want)
			}
		})
	}
}

func generateTestTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"Test"}},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}
}

func TestProbeRegistryTLS_HTTPS(t *testing.T) {
	tlsConf := generateTestTLSConfig(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsConf)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	go http.Serve(ln, mux)

	result := probeRegistryTLS(context.Background(), ln.Addr().String())
	if result != config.TLSInsecure {
		t.Errorf("expected TLSInsecure for HTTPS server, got %v", result)
	}
}

func TestProbeRegistryTLS_HTTP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	go http.Serve(ln, mux)

	result := probeRegistryTLS(context.Background(), ln.Addr().String())
	if result != config.TLSDisabled {
		t.Errorf("expected TLSDisabled for HTTP server, got %v", result)
	}
}

func TestProbeRegistryTLS_Unreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	result := probeRegistryTLS(context.Background(), addr)
	if result != config.TLSEnabled {
		t.Errorf("expected TLSEnabled for unreachable host, got %v", result)
	}
}

func TestBuildHostConfig(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	go http.Serve(ln, mux)

	imageRef := fmt.Sprintf("%s/myrepo:latest", ln.Addr().String())
	host := buildHostConfig(context.Background(), imageRef, "admin", "secret")

	if host.User != "admin" {
		t.Errorf("User = %q, want %q", host.User, "admin")
	}
	if host.Pass != "secret" {
		t.Errorf("Pass = %q, want %q", host.Pass, "secret")
	}
	if host.TLS != config.TLSDisabled {
		t.Errorf("TLS = %v, want TLSDisabled for HTTP server", host.TLS)
	}
}
