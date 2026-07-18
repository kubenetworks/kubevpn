package core

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestUDPReverseBridge_EndToEnd wires the pod side (ServeUDPReverse) and the dev
// side (DialUDPReverse) over an in-memory net.Pipe, with a loopback UDP echo
// server standing in for the developer's local service. It then sends UDP to the
// "envoy" port and asserts the datagram is bridged to the echo server and back —
// the full fargate UDP inbound path minus the cluster.
func TestUDPReverseBridge_EndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Local UDP echo server (stands in for the dev's udpServer).
	echo, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	localPort := echo.LocalAddr().(*net.UDPAddr).Port
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteToUDP(buf[:n], addr)
		}
	}()

	// Pick a free UDP port for the "envoy" side that ServeUDPReverse will bind.
	tmp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	envoyPort := tmp.LocalAddr().(*net.UDPAddr).Port
	_ = tmp.Close()

	podSide, devSide := net.Pipe()
	defer podSide.Close()
	defer devSide.Close()
	go func() { _ = ServeUDPReverse(ctx, podSide) }()
	go func() { _ = DialUDPReverse(ctx, devSide, envoyPort, localPort) }()

	// "envoy" client dials the bridged UDP port and expects an echo.
	client, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: envoyPort})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var got string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_ = client.SetDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err = client.Write([]byte("ping")); err != nil {
			continue
		}
		buf := make([]byte, 64)
		n, rerr := client.Read(buf)
		if rerr == nil {
			got = string(buf[:n])
			break
		}
	}
	if got != "ping" {
		t.Fatalf("reverse UDP bridge did not echo: got %q want %q", got, "ping")
	}
}
