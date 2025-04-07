package util

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"

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

func getTls(tlsSecret map[string][]byte) (crtBytes []byte, keyBytes []byte, serverName []byte, err error) {
	if tlsSecret != nil {
		crtBytes = tlsSecret[config.TLSCertKey]
		keyBytes = tlsSecret[config.TLSPrivateKeyKey]
		serverName = tlsSecret[config.TLSServerName]
		crtBytes, err = io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(crtBytes)))
		if err != nil {
			return
		}
		keyBytes, err = io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(keyBytes)))
		if err != nil {
			return
		}
		serverName, err = io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(serverName)))
		if err != nil {
			return
		}
		return
	}

	crtBytes = []byte(os.Getenv(config.TLSCertKey))
	keyBytes = []byte(os.Getenv(config.TLSPrivateKeyKey))
	serverName = []byte(os.Getenv(config.TLSServerName))
	return
}

func GetTLSHost(ns string) string {
	return fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, ns)
}

func GenTLSCert(ctx context.Context, host string) ([]byte, []byte, []byte, error) {
	ip := net.IPv4(127, 0, 0, 1)
	alternateDNS := "localhost"
	crt, key, err := cert.GenerateSelfSignedCertKeyWithFixtures(host, []net.IP{ip}, []string{alternateDNS}, ".")
	if err != nil {
		log.G(ctx).Errorf("Generate self signed cert and key error: %s", err.Error())
		return nil, nil, nil, err
	}

	// ref --start vendor/k8s.io/client-go/util/cert/cert.go:113
	_ = os.Remove(fmt.Sprintf("%s_%s_%s.crt", host, ip, alternateDNS))
	_ = os.Remove(fmt.Sprintf("%s_%s_%s.key", host, ip, alternateDNS))
	// ref --end
	return crt, key, []byte(host), nil
}

func Base64DecodeToString(base64Data []byte) string {
	crtBytes, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(base64Data)))
	if err != nil {
		return ""
	}
	return string(crtBytes)
}
