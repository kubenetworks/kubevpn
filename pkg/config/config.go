package config

import (
	"net"
	"runtime"
	"sync"
	"time"

	"sigs.k8s.io/kustomize/api/konfig"
)

const (
	// ConfigMapPodTrafficManager is the name of the ConfigMap used by the traffic manager.
	ConfigMapPodTrafficManager = "kubevpn-traffic-manager"

	// HelmAppNameKubevpn is the Helm release name for kubevpn.
	HelmAppNameKubevpn = "kubevpn"

	// DefaultNamespaceKubevpn is the default namespace where kubevpn is installed.
	DefaultNamespaceKubevpn = "kubevpn"

	// KeyTunIPPool is the ConfigMap key for the TUN IP address pool (dual-stack bitmap).
	// Replaces the former DHCP + DHCP6 keys.
	KeyTunIPPool = "TUN_IP_POOL"
	// KeyEnvoy is the ConfigMap key for the Envoy proxy configuration.
	KeyEnvoy = "ENVOY_CONFIG"
	// KeyAgentXDS stores manager-derived per-node Virtuals for xDS consumers.
	// ENVOY_CONFIG remains the desired base Virtual list.
	KeyAgentXDS = "AGENT_XDS_CONFIG"
	// KeyNodeAttachments stores desired per-pod node-boundary attachments.
	KeyNodeAttachments = "NODE_ATTACHMENTS"
	// KeyAgentPortAllocations persists listener assignments and tombstones so
	// leave/re-proxy cannot reuse a port whose old Envoy is still exiting.
	KeyAgentPortAllocations = "AGENT_PORT_ALLOCATIONS"
	// KeyClusterCIDRs is the ConfigMap key for cluster CIDR cache (IPv4 + IPv6).
	KeyClusterCIDRs = "CLUSTER_CIDRS"
	// KeyClusterCIDRsSchema is the ConfigMap key holding the schema version of the
	// CLUSTER_CIDRs cache. The manager warm-up stamps it with CurrentClusterCIDRsSchema
	// whenever it writes the CIDR cache. A populated CIDR cache whose schema is older
	// than current (or absent — written by a pre-versioning manager/client) is treated
	// as STALE and re-validated/overwritten on the next warm-up; a current-schema cache
	// is never overwritten (protects manual edits). See docs/32 §6, docs/46.
	KeyClusterCIDRsSchema = "CLUSTER_CIDRS_SCHEMA"
	// CurrentClusterCIDRsSchema is the current CIDR-cache schema version. Bumped when a
	// detection fix changes what a correct cache looks like, so a manager upgrade can
	// auto-recover caches the previous (buggy) version wrote — e.g. v1 under-detected
	// GKE's Service CIDR (/23 instead of /20); v2 uses the server-side dry-run probe
	// that returns the authoritative range. Absent key reads as 0 (< current).
	CurrentClusterCIDRsSchema = "2"
	// KeyClusterDNS is the ConfigMap key holding the manager pod's /etc/resolv.conf
	// (raw text). The manager publishes it so clients read the cluster DNS server +
	// search domains from the cache instead of exec-ing into the pod. See docs/46.
	KeyClusterDNS = "CLUSTER_DNS_RESOLV"
	// KeyTunAllocs is the ConfigMap key for TUN IP owner allocations (ownerID → IPs).
	// It is a server-written crash-recovery journal (saveAllocs) AND the operator's
	// manual-IP control input: editing an owner's ipv4/ipv6 (keeping the version field,
	// as `kubectl edit` does) proposes a new TUN IP. The reconcile ignores entries whose
	// version is older than the in-memory allocation — those are stale reads of the
	// server's own journal writes, not operator edits — which prevents an oscillation.
	KeyTunAllocs = "TUN_ALLOCS"

	// TLSCertKey is the key for tls certificates in a TLS secret.
	TLSCertKey = "tls_crt"
	// TLSPrivateKeyKey is the key for the private key field in a TLS secret.
	TLSPrivateKeyKey = "tls_key"
	// TLSServerName for tls config server name
	TLSServerName = "tls_server_name"
	// ObfuscationKey is the secret key and environment variable name that
	// opts OUT of data-plane stream obfuscation. Obfuscation is on by
	// default; a value of "0"/"false"/"off" on either endpoint disables it.
	// Both endpoints read the same source (Secret key, or env via EnvFrom),
	// so they always agree.
	ObfuscationKey = "KUBEVPN_OBFUSCATE"

	// ContainerSidecarEnvoy is the container name for the Envoy proxy sidecar.
	ContainerSidecarEnvoy = "envoy"
	// ContainerSidecarXDS is the container name for the xDS server sidecar.
	ContainerSidecarXDS = "xds"
	// ContainerSidecarDNS is the container name for the DNS forward server sidecar.
	ContainerSidecarDNS = "dns"
	// ContainerSidecarVPN is the container name for the VPN sidecar.
	ContainerSidecarVPN = "vpn"
	// ContainerSidecarSyncthing is the container name for the Syncthing file-sync sidecar.
	ContainerSidecarSyncthing = "syncthing"

	// VolumeSyncthing is the volume name for the Syncthing shared directory.
	VolumeSyncthing = "syncthing"

	// PortNameTCP is the traffic-manager service port name for TCP tunneling.
	PortNameTCP = "10801-for-tcp"
	// PortTCP is the traffic-manager TCP tunnel port number.
	PortTCP = 10801
	// PortNameEnvoy is the traffic-manager service port name for the Envoy control plane.
	PortNameEnvoy = "9002-for-envoy"
	// PortXDS is the xDS gRPC port number.
	PortXDS = 9002
	// PortNameDNS is the traffic-manager service port name for DNS resolution.
	PortNameDNS = "53-for-dns"
	// PortDNS is the traffic-manager DNS service port number.
	PortDNS = 53
	// PortNameUDP is the traffic-manager service port name for UDP tunneling (over TCP).
	PortNameUDP = "10802-for-udp"
	// PortUDP is the traffic-manager UDP tunnel port number.
	PortUDP = 10802
	// IPv4Pool is the TUN IP allocation range. Its prefix bounds the allocator
	// (InRange); individual leases carry a host mask (/32), not this pool prefix.
	// 198.18.0.0/16 network is part of the 198.18.0.0/15 (reserved for benchmarking).
	// https://www.iana.org/assignments/iana-ipv4-special-registry/iana-ipv4-special-registry.xhtml
	// so we split it into 2 parts: 198.18.0.0/15 --> [198.18.0.0/16, 198.19.0.0/16]
	IPv4Pool = "198.18.0.0/16"
	// IPv6Pool is the IPv6 TUN IP allocation range. As with IPv4Pool, its prefix
	// bounds the allocator while individual leases carry a host mask (/128).
	// 2001:2::/64 network is part of the 2001:2::/48 (reserved for benchmarking)
	// https://www.iana.org/assignments/iana-ipv6-special-registry/iana-ipv6-special-registry.xhtml
	IPv6Pool = "2001:2::/64"
	// IPv4UserPool is the lower half of IPv4Pool reserved for user connections.
	// Range: 198.18.0.1 - 198.18.127.254
	IPv4UserPool = "198.18.0.0/17"
	// IPv4AgentPool is the upper half of IPv4Pool reserved for agent connections.
	// Range: 198.18.128.1 - 198.18.255.254
	IPv4AgentPool = "198.18.128.0/17"
	// IPv6UserPool is the lower half of IPv6Pool reserved for user connections.
	// Range: 2001:2:: - 2001:2::7fff:ffff:ffff:ffff
	IPv6UserPool = "2001:2::/65"
	// IPv6AgentPool is the upper half of IPv6Pool reserved for agent connections.
	// Range: 2001:2::8000:0:0:0 - 2001:2::ffff:ffff:ffff:ffff
	IPv6AgentPool = "2001:2::8000:0:0:0/65"
	// DockerIPv4Pool is the IPv4 CIDR used for kubevpn Docker bridge networks.
	/*
		reason：docker use 172.17.0.0/16 network conflict with k8s service kubernetes
		➜  ~ kubectl get service kubernetes
		NAME         TYPE        CLUSTER-IP   EXTERNAL-IP   PORT(S)   AGE
		kubernetes   ClusterIP   172.17.0.1   <none>        443/TCP   190d

		➜  ~ docker network inspect bridge | jq '.[0].IPAM.Config'
		[
		 {
		   "Subnet": "172.17.0.0/16",
		   "Gateway": "172.17.0.1"
		 }
		]
	*/
	DockerIPv4Pool = "198.19.0.1/16"

	// EnvInboundPodTunIPv4 is the env var name for the pod's TUN device IPv4 address.
	EnvInboundPodTunIPv4 = "TunIPv4"
	// EnvInboundPodTunIPv6 is the env var name for the pod's TUN device IPv6 address.
	EnvInboundPodTunIPv6 = "TunIPv6"
	// EnvPodName is the env var name populated with the Kubernetes pod name.
	EnvPodName = "POD_NAME"
	// EnvPodNamespace is the env var name populated with the Kubernetes pod namespace.
	EnvPodNamespace = "POD_NAMESPACE"

	// KUBECONFIG is the key name used when storing kubeconfig data.
	KUBECONFIG = "kubeconfig"

	// ManageBy is the label key indicating the resource is managed by kubevpn.
	ManageBy = konfig.ManagedbyLabelKey

	// PProfPort is the pprof HTTP server port for the user daemon.
	PProfPort = 32345
	// SudoPProfPort is the pprof HTTP server port for the root daemon.
	SudoPProfPort = 33345
	// PProfDir is the directory name for storing pprof profiles.
	PProfDir = "pprof"

	// EnvSSHJump is the env var set when connected via an SSH jump host.
	EnvSSHJump = "SSH_JUMP_BY_KUBEVPN"

	// HostsKeyword is the marker comment appended to /etc/hosts entries managed by kubevpn.
	HostsKeyword = "Added by KubeVPN"
	// HostsDeviceKeyword is the per-device marker comment template for /etc/hosts entries.
	HostsDeviceKeyword = "# For dev %s " + HostsKeyword

	// AgentIptablesComment is the comment marker for node-agent iptables rules.
	AgentIptablesComment = "kubevpn-nodeagent"
	// AgentDNATRuleSpec4 is the IPv4 DNAT rule spec (without -t nat -A PREROUTING) for node-agent.
	// Keep in sync with AgentDNATRuleSpec6 and the bash script in pkg/inject/nodeagent_job.go.
	AgentDNATRuleSpec4 = "-p tcp ! -s 127.0.0.1 ! -d ${CIDR4} -j DNAT --to :15006 -m comment --comment \"kubevpn-nodeagent\""
	// AgentDNATRuleSpec6 is the IPv6 DNAT rule spec (without -t nat -A PREROUTING) for node-agent.
	// Keep in sync with AgentDNATRuleSpec4 and the bash script in pkg/inject/nodeagent_job.go.
	AgentDNATRuleSpec6 = "-p tcp ! -s ::1 ! -d ${CIDR6} -j DNAT --to :15006 -m comment --comment \"kubevpn-nodeagent\""
)

var (
	// Image is the container image used for injected sidecars, set via -ldflags.
	Image = "ghcr.io/kubenetworks/kubevpn:latest"
	// Version is the build version string, set via -ldflags.
	Version = "latest"
	// GitCommit is the git commit SHA at build time, set via -ldflags.
	GitCommit = ""

	// GitHubOAuthToken is the GitHub OAuth token for release checks, set via -ldflags.
	GitHubOAuthToken = ""

	// OriginImage is the unmodified container image tag derived from Version.
	OriginImage = "ghcr.io/kubenetworks/kubevpn:" + Version
)

var (
	// CIDR is the parsed IPv4 network used for TUN address allocation.
	CIDR *net.IPNet
	// CIDR6 is the parsed IPv6 network used for TUN address allocation.
	CIDR6 *net.IPNet
	// CIDRUser is the parsed IPv4 network used for user TUN address allocation.
	CIDRUser *net.IPNet
	// CIDRAgent is the parsed IPv4 network used for agent TUN address allocation.
	CIDRAgent *net.IPNet
	// CIDR6User is the parsed IPv6 network used for user TUN address allocation.
	CIDR6User *net.IPNet
	// CIDR6Agent is the parsed IPv6 network used for agent TUN address allocation.
	CIDR6Agent *net.IPNet
	// RouterIP is the gateway IPv4 address within CIDR.
	RouterIP net.IP
	// RouterIP6 is the gateway IPv6 address within CIDR6.
	RouterIP6 net.IP

	// DockerCIDR is the IPv4 network used for the kubevpn Docker bridge.
	DockerCIDR *net.IPNet
	// DockerRouterIP is the gateway IPv4 address within DockerCIDR.
	DockerRouterIP net.IP
)

func init() {
	var err error
	RouterIP, CIDR, err = net.ParseCIDR(IPv4Pool)
	if err != nil {
		panic(err)
	}
	RouterIP6, CIDR6, err = net.ParseCIDR(IPv6Pool)
	if err != nil {
		panic(err)
	}
	_, CIDRUser, err = net.ParseCIDR(IPv4UserPool)
	if err != nil {
		panic(err)
	}
	_, CIDRAgent, err = net.ParseCIDR(IPv4AgentPool)
	if err != nil {
		panic(err)
	}
	_, CIDR6User, err = net.ParseCIDR(IPv6UserPool)
	if err != nil {
		panic(err)
	}
	_, CIDR6Agent, err = net.ParseCIDR(IPv6AgentPool)
	if err != nil {
		panic(err)
	}
	DockerRouterIP, DockerCIDR, err = net.ParseCIDR(DockerIPv4Pool)
	if err != nil {
		panic(err)
	}
}

// Debug enables verbose debug logging when true.
var Debug bool

var (
	// LargeBufferSize is the byte size for large I/O buffers (64KB).
	LargeBufferSize = 64 * 1024
)

var (
	KeepAliveTime  = 60 * time.Second
	DialTimeout    = 15 * time.Second
	ConnectTimeout = 5 * time.Second

	// HeartbeatInterval is how often the client sends its data-plane ICMP heartbeat. It is
	// deliberately shorter than KeepAliveTime (which drives the read/write deadlines and TCP
	// keepalive): the heartbeat echo reply is the data-plane liveness signal that drives the
	// port-forward black-hole watchdog, so a short cadence lets a silent tunnel be detected in
	// seconds. Sending more often only helps the server read deadline (KeepAliveTime*3), never
	// hurts it.
	HeartbeatInterval = 5 * time.Second

	DNSRouteRefreshInterval  = 15 * time.Second
	DNSRouteDebounceInterval = 3 * time.Second

	UDPSessionTimeout = 120 * time.Second
	UDPRelayTimeout   = 60 * time.Second

	SlotReconnectBackoff = 2 * time.Second
	DaemonPollInterval   = 200 * time.Millisecond

	// DaemonProbeTimeout bounds local health, version, and identity RPCs. It does
	// not apply to long-lived streams or daemon cleanup.
	DaemonProbeTimeout = 2 * time.Second
	// DaemonQuitTimeout bounds version-mismatch cleanup, which may tear down
	// multiple connections and must outlive the short bootstrap probe deadline.
	DaemonQuitTimeout = 2 * time.Minute
	// DaemonStartupLockTimeout bounds waiters behind another CLI's interactive
	// elevation and launch. The lock holder may still wait for user authentication.
	DaemonStartupLockTimeout = 35 * time.Second
	// DaemonExitTimeout bounds waiting for an outdated daemon to finish cleanup.
	DaemonExitTimeout = 30 * time.Second

	// RolloutStatusTimeout bounds how long RolloutStatus waits for a workload
	// rollout to finish. Without it, a sidecar that never becomes ready makes
	// the operation (e.g. `kubevpn proxy`) hang indefinitely.
	RolloutStatusTimeout = 2 * time.Minute

	// ConfigMapSyncTimeout bounds how long ConfigMapStore.EnsureSynced waits for the
	// traffic manager ConfigMap informer to complete its initial List at connection
	// establishment. The store has no live-API read fallback, so reads before the cache
	// is warm return empty; warming here keeps steady-state reads cache-served. Connect
	// treats a timeout as fatal (fail fast) — a cache that never warms would read empty
	// forever, and a sync failure here means the API is already unreachable.
	ConfigMapSyncTimeout = 10 * time.Second

	// SudoStatusTimeout bounds the user daemon's cross-daemon Status hop to the sudo
	// daemon (getSudoTunIPs). It caps how long a wedged sudo daemon can stall an
	// otherwise-local command, independent of the read-path fixes.
	SudoStatusTimeout = 2 * time.Second

	// ManagerServiceGetTimeout bounds each direct-to-apiserver call made while reaching or
	// tearing down the traffic manager (Service lookup, manager Job delete). Without it each
	// inherits client-go's ~30s default dial timeout, so a disconnect/quit against an
	// unreachable cluster (e.g. deleted) stacks several 30s stalls. A manager Service GET on a
	// reachable cluster returns in well under a second, so a few seconds is ample; teardown is
	// best-effort (the manager's lease reaper reclaims anything left), so failing fast is safe.
	ManagerServiceGetTimeout = 3 * time.Second

	// SudoLivenessProbeInterval is how often the user daemon's MonitorSudoLiveness probes the
	// root (sudo) daemon to keep its cached data-plane health snapshot fresh. Matched to the
	// self-staleness watchdog's poll cadence (detectUnixSocksFile), so a crashed root daemon is
	// reflected in `kubevpn status` within ~1 interval — far ahead of the 90s heartbeat window.
	SudoLivenessProbeInterval = 2 * time.Second
)

var (
	// DefaultMTU is the TUN device MTU, calculated to fit within a 1500-byte Ethernet frame after IP/TCP/TLS overhead.
	/**
	  +--------------------------------------------------------------------+
	  |                     Original IP Packet from TUN                    |
	  +-------------------+------------------------------------------------+
	  |  IP Header (20B)  |               Payload (MTU size)               |
	  +-------------------+------------------------------------------------+


	  After adding custom 2-byte header:
	  +----+-------------------+-------------------------------------------+
	  | LH |  IP Header (20B)  |               Payload                     |
	  +----+-------------------+-------------------------------------------+
	  | 2B |        20B        |          1453 - 20 = 1433B                |
	  +----+-------------------+-------------------------------------------+

	  TLS 1.3 Record Structure Breakdown:
	  +---------------------+--------------------------+-------------------+
	  | TLS Header (5B)     | Encrypted Data (N)       | Auth Tag (16B)    |
	  +---------------------+--------------------------+-------------------+
	  |  Content Type (1)   |          ↑               | AEAD Authentication
	  |   Version (2)       |   Encrypted Payload      | (e.g. AES-GCM)    |
	  |   Length (2)        |  (Original Data + LH2)   |                   |
	  +---------------------+--------------------------+-------------------+
	  |←------- 5B --------→|←---- Length Field ------→|←----- 16B -------→|


	  Final Ethernet Frame:
	  +--------+----------------+----------------+-----------------------+--------+
	  | EthHdr | IP Header      | TCP Header     | TLS Components                 |
	  | (14B)  | (20B)          | (20B)          +---------+-------------+--------+
	  |        |                |                | Hdr(5B) | Data+LH2    | Tag(16)|
	  +--------+----------------+----------------+---------+-------------+--------+
	  |←------------------- Total 1500B Ethernet Frame --------------------------→|

	  ipv4: 20
	  ipv6: 40

	mtu = 1500 - ip header(20/40 v4/v6) - tcp header (20) - tls1.3(5+1+16) - packet over tcp(length(2)+remark(1)) = 1415
	*/
	DefaultMTU = 1500 - max(20, 40) - 20 - (5 + 1 + 16) - (2 + 1)
)

var (
	// LPool is a sync.Pool of LargeBufferSize byte slices for reuse in I/O paths.
	LPool = sync.Pool{
		New: func() any {
			if runtime.GOOS == "linux" {
				// Keep the logical frame limit, with capacity for a full native
				// read segment at offset 3 (65535+3). Heap rounding costs more
				// than these two capacity bytes.
				return make([]byte, LargeBufferSize, LargeBufferSize+2)
			}
			return make([]byte, LargeBufferSize)
		},
	}
)

// Slogan is the success message printed after a connection is established.
const Slogan = "Connected. You can now access the Kubernetes cluster."
