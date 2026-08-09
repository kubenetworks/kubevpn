package netutil

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"k8s.io/client-go/util/cert"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// GetTlsClientConfig builds a TLS client configuration from the certificate, key, and server name in tlsSecret.
func GetTlsClientConfig(tlsSecret map[string][]byte) (*tls.Config, error) {
	crtBytes, keyBytes, serverName, err := getTls(tlsSecret)
	if err != nil {
		return nil, err
	}

	pair, err := tls.X509KeyPair(crtBytes, keyBytes)
	if err != nil {
		return nil, err
	}
	certPool := x509.NewCertPool()
	certPool.AppendCertsFromPEM(crtBytes)
	return &tls.Config{
		RootCAs:      certPool,
		Certificates: []tls.Certificate{pair},
		ServerName:   string(serverName),
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
	}, nil
}

// GetTlsServerConfig builds a TLS server configuration from the certificate, key, and server name in tlsInfo.
func GetTlsServerConfig(tlsInfo map[string][]byte) (*tls.Config, error) {
	crtBytes, keyBytes, serverName, err := getTls(tlsInfo)
	if err != nil {
		return nil, err
	}

	pair, err := tls.X509KeyPair(crtBytes, keyBytes)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		ServerName:   string(serverName),
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
	}, nil
}

var ErrNoTLSConfig = errors.New("no TLS configuration found")

func getTls(tlsSecret map[string][]byte) (crtBytes []byte, keyBytes []byte, serverName []byte, err error) {
	if tlsSecret != nil {
		crtBytes = tlsSecret[config.TLSCertKey]
		keyBytes = tlsSecret[config.TLSPrivateKeyKey]
		serverName = tlsSecret[config.TLSServerName]
		return
	}

	if os.Getenv(config.TLSCertKey) == "" ||
		os.Getenv(config.TLSPrivateKeyKey) == "" ||
		os.Getenv(config.TLSServerName) == "" {
		return nil, nil, nil, ErrNoTLSConfig
	}

	crtBytes = []byte(os.Getenv(config.TLSCertKey))
	keyBytes = []byte(os.Getenv(config.TLSPrivateKeyKey))
	serverName = []byte(os.Getenv(config.TLSServerName))
	return
}

// GetTLSHost returns the TLS server name for the traffic manager service in the given namespace.
func GetTLSHost(ns string) string {
	return fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, ns)
}

// GenTLSCert generates a self-signed TLS certificate and key for the traffic manager in the given namespace.
func GenTLSCert(ctx context.Context, ns string) ([]byte, []byte, []byte, error) {
	host := GetTLSHost(ns)
	alternateIPs := []net.IP{net.IPv4(127, 0, 0, 1)}
	alternateDNS := []string{"localhost"}
	crt, key, err := cert.GenerateSelfSignedCertKeyWithFixtures(host, alternateIPs, alternateDNS, ".")
	if err != nil {
		plog.G(ctx).Errorf("Generate self signed cert and key error: %v", err)
		return nil, nil, nil, err
	}

	// ref --start vendor/k8s.io/client-go/util/cert/cert.go:113
	baseName := fmt.Sprintf("%s_%s_%s", host, strings.Join(ipsToStrings(alternateIPs), "-"), strings.Join(alternateDNS, "-"))
	_ = os.Remove(baseName + ".crt")
	_ = os.Remove(baseName + ".key")
	// ref --end
	return crt, key, []byte(host), nil
}

func ipsToStrings(ips []net.IP) []string {
	ss := make([]string, 0, len(ips))
	for _, ip := range ips {
		ss = append(ss, ip.String())
	}
	return ss
}
