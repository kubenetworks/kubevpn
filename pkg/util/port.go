package util

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
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

func getAvailableUDPPort() (int, error) {
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

// GetAvailableTCPPort returns an available TCP port on localhost by briefly binding to port 0.
func GetAvailableTCPPort() (int, error) {
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

func isPortListening(port int) bool {
	listener, err := net.Listen("tcp4", net.JoinHostPort("localhost", strconv.Itoa(port)))
	if err != nil {
		return true
	} else {
		_ = listener.Close()
		return false
	}
}
