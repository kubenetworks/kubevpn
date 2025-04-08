package util

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
	"github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

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
	client := &tls.Config{
		RootCAs:      certPool,
		Certificates: []tls.Certificate{pair},
		ServerName:   string(serverName),
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
	}
	return client, nil
}

func GetTlsServerConfig(tlsInfo map[string][]byte) (*tls.Config, error) {
	crtBytes, keyBytes, serverName, err := getTls(tlsInfo)
	if err != nil {
		return nil, err
	}

	pair, err := tls.X509KeyPair(crtBytes, keyBytes)
	if err != nil {
		return nil, err
	}
	client := &tls.Config{
		Certificates: []tls.Certificate{pair},
		ServerName:   string(serverName),
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
	}
	return client, nil
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

func GetTLSHost(ns string) string {
	return fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, ns)
}

func GenTLSCert(ctx context.Context, ns string) ([]byte, []byte, []byte, error) {
	host := GetTLSHost(ns)
	alternateIPs := []net.IP{net.IPv4(127, 0, 0, 1)}
	alternateDNS := []string{"localhost"}
	// for Mutatingwebhookconfigurations will use domain: kubevpn-traffic-manager.xxx.svc
	alternateDNS = append(alternateDNS, fmt.Sprintf("%s.svc", host))
	crt, key, err := cert.GenerateSelfSignedCertKeyWithFixtures(host, alternateIPs, alternateDNS, ".")
	if err != nil {
		log.G(ctx).Errorf("Generate self signed cert and key error: %s", err.Error())
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
