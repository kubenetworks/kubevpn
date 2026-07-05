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
	remoteStr, localStr, hasLocal := strings.Cut(str, ":")
	remote, _ = strconv.Atoi(remoteStr)
	if hasLocal {
		local, _ = strconv.Atoi(localStr)
	} else {
		local = remote
	}
	return v1.ContainerPort{
		HostPort:      int32(local),
		ContainerPort: int32(remote),
		Protocol:      protocol,
	}
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

// GetAvailableUDPPort returns an available UDP port on localhost by briefly binding to port 0.
func GetAvailableUDPPort() (int, error) {
	address, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:0", "localhost"))
	if err != nil {
		return 0, err
	}
	conn, err := net.ListenUDP("udp", address)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).Port, nil
}
