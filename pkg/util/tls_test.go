package util

import (
	"context"
	"crypto/tls"
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
