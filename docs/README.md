# KubeVPN Architecture & Design Docs

**Note:**
- User documentation (installation, commands, FAQ, tutorials): [kubevpn.dev](https://kubevpn.dev)
- Implementation & architecture design documents (this directory): for contributors and maintainers

## Getting Started

- [01-network-architecture.md](./01-network-architecture.md): Overall network architecture overview
- [02-dual-daemon.md](./02-dual-daemon.md): User daemon (control plane) vs root daemon (data plane)
- [10-handler-architecture.md](./10-handler-architecture.md): Core handler package design
- [12-session-lifecycle.md](./12-session-lifecycle.md): Session lifecycle management
- [35-daemon-bootstrap.md](./35-daemon-bootstrap.md): Daemon startup/bootstrap process

## Connection & Data Plane

- [03-dhcp-ip-allocation.md](./03-dhcp-ip-allocation.md): TUN IP allocation via ConfigMap
- [04-connection-id.md](./04-connection-id.md): Connection ID generation and usage
- [09-tun-ip-hot-update.md](./09-tun-ip-hot-update.md): Hot updating TUN IP without disconnect
- [22-tun-device.md](./22-tun-device.md): TUN device management across platforms
- [28-sleep-wake-ip-update.md](./28-sleep-wake-ip-update.md): Handling sleep/wake cycles
- [29-stable-tun-ip.md](./29-stable-tun-ip.md): Stable TUN IP across reconnects
- [32-cidr-detection.md](./32-cidr-detection.md): Cluster CIDR detection
- [38-ipv6-dual-stack.md](./38-ipv6-dual-stack.md): IPv4/IPv6 dual-stack support
- [44-server-side-route-discovery.md](./44-server-side-route-discovery.md): Server-side route discovery
- [45-tun-ip-change-stale-address.md](./45-tun-ip-change-stale-address.md): Handling stale addresses after IP change
- [46-server-side-cidr-dns-detection.md](./46-server-side-cidr-dns-detection.md): Server-side CIDR/DNS detection
- [51-idle-client-route-registration.md](./51-idle-client-route-registration.md): Idle client route registration
- [52-stale-route-src-after-ip-change.md](./52-stale-route-src-after-ip-change.md): Stale route source after IP change

## Proxy & Envoy (Service Mesh)

- [05-owner-id.md](./05-owner-id.md): Envoy rule ownership by OwnerID
- [06-fargate-mode.md](./06-fargate-mode.md): Fargate/Service mode design
- [16-envoy-controlplane.md](./16-envoy-controlplane.md): Envoy control plane (xDS)
- [17-sidecar-injection.md](./17-sidecar-injection.md): Sidecar injection strategies
- [40-envoy-original-dst-loop.md](./40-envoy-original-dst-loop.md): Envoy original destination loop
- [41-five-tuple-pool-dispatch.md](./41-five-tuple-pool-dispatch.md): 5-tuple pool dispatch
- [42-origin-loopback-cluster.md](./42-origin-loopback-cluster.md): Origin loopback cluster

## Sync & Local Development

- [20-kubevpn-run.md](./20-kubevpn-run.md): `kubevpn run` local Docker mode
- [23-syncthing-file-sync.md](./23-syncthing-file-sync.md): Syncthing-based file sync
- [26-sync-mode.md](./26-sync-mode.md): Sync mode design
- [27-image-copy.md](./27-image-copy.md): Image copy functionality
- [36-workload-restore.md](./36-workload-restore.md): Workload restore after proxy leave

## Upgrade & Operations

- [33-client-upgrade.md](./33-client-upgrade.md): Client self-upgrade mechanism
- [37-uninstall-cleanup.md](./37-uninstall-cleanup.md): Uninstall and cleanup procedures
- [47-portforward-blackhole-liveness.md](./47-portforward-blackhole-liveness.md): Port-forward blackhole liveness check

## Security & Networking

- [08-heartbeat-health.md](./08-heartbeat-health.md): Heartbeat and health checking
- [15-ssh-architecture.md](./15-ssh-architecture.md): SSH jump host architecture
- [18-gvisor-network-stack.md](./18-gvisor-network-stack.md): gVisor network stack usage
- [19-dns-resolution.md](./19-dns-resolution.md): DNS resolution setup
- [21-local-proxy.md](./21-local-proxy.md): Local SOCKS5 proxy
- [34-privilege-escalation.md](./34-privilege-escalation.md): Privilege escalation (sudo)
- [48-shared-server-gvisor-stack.md](./48-shared-server-gvisor-stack.md): Shared server gVisor stack
- [49-ssh-dataplane-bypass.md](./49-ssh-dataplane-bypass.md): SSH data plane bypass
- [53-ebpf-feasibility.md](./53-ebpf-feasibility.md): eBPF feasibility study
- [54-traffic-obfuscation.md](./54-traffic-obfuscation.md): Traffic obfuscation and TLS enforcement

## Internals & Observability

- [07-namespace-model.md](./07-namespace-model.md): Namespace model
- [11-configmap-informer.md](./11-configmap-informer.md): ConfigMap informer
- [13-logging-architecture.md](./13-logging-architecture.md): Logging architecture
- [14-rpc-daemon-mapping.md](./14-rpc-daemon-mapping.md): RPC to daemon mapping
- [24-traffic-manager-deployment.md](./24-traffic-manager-deployment.md): Traffic manager deployment
- [25-file-copy.md](./25-file-copy.md): File copy (kubectl cp equivalent)
- [30-connect-progress.md](./30-connect-progress.md): Connect progress reporting
- [31-exit-codes.md](./31-exit-codes.md): Exit code definitions
- [39-config-alias.md](./39-config-alias.md): Config alias functionality
- [50-crossclient-throughput-diagnosis.md](./50-crossclient-throughput-diagnosis.md): Cross-client throughput diagnosis
- [43-macos-ci-apiserver-flakiness.md](./43-macos-ci-apiserver-flakiness.md): macOS CI apiserver flakiness mitigation
