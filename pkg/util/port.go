package util

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// ParsePort [tcp/udp]/remote:local
func ParsePort(str string) v1.ContainerPort {
	protocol := v1.ProtocolTCP
	if i := strings.Index(str, "/"); i != -1 {
		switch strings.ToLower(str[:i]) {
		case "udp":
			protocol = v1.ProtocolUDP
		default:
			protocol = v1.ProtocolTCP
		}
		str = str[i+1:]
	}
	var local, remote int
	ports := strings.Split(str, ":")
	if len(ports) == 2 {
		remote, _ = strconv.Atoi(ports[0])
		local, _ = strconv.Atoi(ports[1])
	} else {
		remote, _ = strconv.Atoi(ports[0])
		local = remote
	}
	return v1.ContainerPort{
		HostPort:      int32(local),
		ContainerPort: int32(remote),
		Protocol:      protocol,
	}
}

func GetAvailableUDPPortOrDie() (int, error) {
	address, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:0", "localhost"))
	if err != nil {
		return 0, err
	}
	listener, err := net.ListenUDP("udp", address)
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.LocalAddr().(*net.UDPAddr).Port, nil
}

func GetAvailableTCPPortOrDie() (int, error) {
	address, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:0", "localhost"))
	if err != nil {
		return 0, err
	}
	listener, err := net.ListenTCP("tcp", address)
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func WaitPortToBeFree(ctx context.Context, port int) error {
	plog.G(ctx).Infoln(fmt.Sprintf("Wait port %v to be free...", port))
	ticker := time.NewTicker(time.Second * 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait port %d to be free timeout", port)
		case <-ticker.C:
			if !IsPortListening(port) {
				plog.G(ctx).Infof("Port %v are free", port)
				return nil
			}
		}
	}
}

func IsPortListening(port int) bool {
	listener, err := net.Listen("tcp4", net.JoinHostPort("localhost", strconv.Itoa(port)))
	if err != nil {
		return true
	} else {
		_ = listener.Close()
		return false
	}
}
