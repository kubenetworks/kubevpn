package core

import (
	"context"
	"crypto/tls"
	"k8s.io/client-go/util/cert"
	"net"
	"testing"
	"time"
)

func batchTLSConfig(t testing.TB) *tls.Config {
	t.Helper()
	cp, kp, err := cert.GenerateSelfSignedCertKeyWithFixtures("localhost", []net.IP{net.IPv4(127, 0, 0, 1)}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := tls.X509KeyPair(cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{key}, MinVersion: tls.VersionTLS13}
}

func batchTCPPair(t testing.TB, encrypted bool) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s, err := ln.Accept()
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	if !encrypted {
		return c, s
	}
	server := tls.Server(s, batchTLSConfig(t))
	client := tls.Client(c, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}) // test-only loopback certificate
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { done <- server.HandshakeContext(ctx) }()
	clientErr := client.HandshakeContext(ctx)
	if clientErr != nil {
		c.Close()
		s.Close()
	}
	serverErr := <-done // join even when the client handshake failed
	if clientErr != nil || serverErr != nil {
		t.Fatalf("TLS handshake: client=%v server=%v", clientErr, serverErr)
	}
	return client, server
}
