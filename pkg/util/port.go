package util

import (
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
