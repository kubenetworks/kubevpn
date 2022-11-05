package config

import (
	"net"
	"time"
)

const (
	// configmap name
	ConfigMapPodTrafficManager = "kubevpn-traffic-manager"

	// config map keys
	KeyDHCP             = "DHCP"
	KeyEnvoy            = "ENVOY_CONFIG"
	KeyClusterIPv4POOLS = "IPv4_POOLS"

	// container name
	ContainerSidecarEnvoyProxy   = "envoy-proxy"
	ContainerSidecarControlPlane = "control-plane"
	ContainerSidecarVPN          = "vpn"

	VolumeEnvoyConfig = "envoy-config"

	innerIPv4Pool = "223.254.254.100/24"

	DefaultNetDir = "/etc/cni/net.d"

	Proc = "/proc"
)

var (
	// Version inject --ldflags -X
	Version = "latest"

	ImageServer       = "naison/kubevpn:" + Version
	ImageMesh         = "naison/kubevpn-mesh:" + Version
	ImageControlPlane = "naison/envoy-xds-server:" + Version
)

var CIDR *net.IPNet

var RouterIP net.IP

func init() {
	RouterIP, CIDR, _ = net.ParseCIDR(innerIPv4Pool)
}

var Debug bool

var (
	SmallBufferSize  = 2 * 1024  // 2KB small buffer
	MediumBufferSize = 8 * 1024  // 8KB medium buffer
	LargeBufferSize  = 32 * 1024 // 32KB large buffer
)

var (
	KeepAliveTime    = 180 * time.Second
	DialTimeout      = 15 * time.Second
	HandshakeTimeout = 5 * time.Second
	ConnectTimeout   = 5 * time.Second
	ReadTimeout      = 10 * time.Second
	WriteTimeout     = 10 * time.Second
)

var (
	//	network layer ip needs 20 bytes
	//	transport layer UDP header needs 8 bytes
	//	UDP over TCP header needs 22 bytes
	DefaultMTU = 1500 - 20 - 8 - 21
)
