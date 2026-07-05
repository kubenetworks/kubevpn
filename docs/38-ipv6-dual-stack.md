# IPv6 / Dual-Stack Architecture

## 1. Overview

KubeVPN is dual-stack end to end: every layer that handles an address handles **both an IPv4 and
an IPv6 address as a pair**. A connection is allocated one address from each pool, the TUN device
carries both, routes and DNS are programmed for both families, the gvisor stack forwards both, and
envoy generates listeners per family. IPv6 is enabled best-effort — where the host or pod has no
IPv6, the v6 half is gracefully skipped (e.g. an IPv4-only envoy template).

This doc is the **cross-cutting overview**: it shows how the v4/v6 pair threads through DHCP → TUN
→ routes → DNS → gvisor → envoy. Each layer's internals live in its own doc; this one ties them
together.

## 2. Address Pools and Globals

Defined in `pkg/config/config.go`:

| Constant / var | Value | Role |
|---|---|---|
| `IPv4Pool` | `198.18.0.0/16` | TUN IPv4 allocation pool (benchmarking-reserved range) |
| `IPv6Pool` | `2001:2::/64` | TUN IPv6 allocation pool (benchmarking-reserved range) |
| `CIDR` / `CIDR6` | parsed pools | the two `*net.IPNet` used everywhere |
| `RouterIP` / `RouterIP6` | gateway IPs | v4/v6 gateway within each pool |

The pools are parsed once in `config.init()`. Both ranges are from blocks reserved for
benchmarking (RFC 2544 / RFC 5180), chosen to avoid colliding with real cluster traffic.

## 3. Per-Layer Dual-Stack Handling

```
DHCP (pkg/dhcp)            RentIP → (IPv4, IPv6) pair from two ipallocator.Range bitmaps
   │                       ReleaseIPs dispatches by ip.To4()==nil
   ▼
NetworkManager (handler)   localTunIPv4 + localTunIPv6 ; resp.IPv6 → v6 CIDR
   │                       ChangeTunIP(newIPv4, newIPv6) hot-updates both
   ▼
TUN device (pkg/tun)       assigns both addresses; per-OS v6 path
   │                       routes + iptables/ip6tables per family
   ▼
gvisor stack (pkg/core)    ipv4.NewProtocol + ipv6.NewProtocol; forwarding on both
   │
   ▼
DNS (pkg/dns)              resolver set for AF_INET and AF_INET6
   │
   ▼
Envoy xDS (controlplane)   LocalTunIPv6 propagated into rules; ipv4 vs dual template
```

### 3.1 DHCP — paired allocation (`pkg/dhcp/dhcp.go`)

The `Manager` holds **two** `ipallocator.Range` bitmaps (v4 over `config.CIDR`, v6 over
`config.CIDR6`). `RentIP`/`RentIPExcluding`/`RentIPPreferring`/`RentSpecificIP` all return and take
a v4+v6 pair. `InRange` and `ReleaseIPs` dispatch by `ip.To4() != nil`. See
[03-dhcp-ip-allocation.md](03-dhcp-ip-allocation.md).

### 3.2 NetworkManager — the v4/v6 pair holder (`pkg/handler/network.go`)

`localTunIPv4` and `localTunIPv6` (`*net.IPNet`) are the data-plane's canonical pair, exposed via
`LocalTunIPv4()`/`LocalTunIPv6()`. When the control plane returns a TUN IP, a non-empty
`resp.IPv6` is parsed into the v6 CIDR; `ChangeTunIP(newIPv4, newIPv6)` hot-updates both without
restarting the stack — see [09-tun-ip-hot-update.md](09-tun-ip-hot-update.md).

### 3.3 TUN device — per-OS v6 assignment (`pkg/tun`)

Both addresses are assigned to the interface. The v6 path is platform-specific:
- **macOS/BSD** (`ip_darwin.go`): `SIOCAIFADDR` only *adds* an alias, so `changeIP` removes the old
  address first to avoid the device carrying two IPs.
- **Windows** (`ip_windows.go`): `winipcfg` LUID-based address management.
- **Linux** (`ip_linux.go`): netlink.

Routing mirrors this: `iptables_linux.go` programs `nat PREROUTING` DNAT for IPv4 and the parallel
`ip6tables` rule for IPv6. See [22-tun-device.md](22-tun-device.md).

### 3.4 gvisor stack — both network protocols (`pkg/core/gvisor_stack.go`)

The stack registers `ipv4.NewProtocol` **and** `ipv6.NewProtocol`, sets per-protocol options on
`ipv6.ProtocolNumber`, and enables forwarding for both via `SetForwardingDefaultAndAllNICs`. See
[18-gvisor-network-stack.md](18-gvisor-network-stack.md).

### 3.5 DNS — both families (`pkg/dns`)

On Windows, `dns_windows.go` calls `luid.SetDNS(AF_INET, …)` and `luid.SetDNS(AF_INET6, …)`, and
flushes both on teardown. Other platforms configure the resolver for both families. See
[19-dns-resolution.md](19-dns-resolution.md).

### 3.6 Envoy xDS — per-family + template selection (`pkg/inject`, `pkg/controlplane`)

`LocalTunIPv6` is carried in `envoyRuleSpec` and propagated into each `Rule` so the control plane
can emit listeners/clusters for the v6 target. The sidecar's bootstrap config has **two embedded
variants** — dual-stack (`envoy.yaml`, `fargate_envoy.yaml`) and IPv4-only (`envoy_ipv4.yaml`,
`fargate_envoy_ipv4.yaml`). The variant is chosen at injection time from
`util.DetectPodSupportIPv6` (see `inject/fargate.go`, `inject/container.go`). See
[16-envoy-controlplane.md](16-envoy-controlplane.md) and
[17-sidecar-injection.md](17-sidecar-injection.md).

## 4. Capability Detection

| Function | Location | Checks |
|---|---|---|
| `util.IsIPv6Enabled` (non-Windows) | `pkg/util/net_others.go` | a non-loopback IPv6 address exists on any interface |
| `util.IsIPv6Enabled` (Windows) | `pkg/util/net_windows.go` | Windows registry IPv6 flag |
| `util.DetectPodSupportIPv6` | `pkg/util` | whether pods in the manager namespace support IPv6 (picks envoy template) |
| `util.IsIPv6(packet)` | `pkg/util/net.go` | inspects the IP version nibble of a raw packet (used in the data plane to route by family) |

When IPv6 is unavailable, allocation/assignment of the v6 half is skipped and the IPv4-only envoy
template is used, so a v4-only host still works.

## 5. Packet-Level Family Dispatch

In the data plane, raw packets are classified by `util.IsIPv6` (first-nibble check) to pick the
right handling path; ICMPv6 packets are built via `util.GenICMPPacketIPv6` for the v6 heartbeat,
paralleling the IPv4 ICMP heartbeat in [08-heartbeat-health.md](08-heartbeat-health.md).

## 6. Related Files

| File | Purpose |
|---|---|
| `pkg/config/config.go` | `IPv4Pool`/`IPv6Pool`, `CIDR`/`CIDR6`, `RouterIP`/`RouterIP6` |
| `pkg/dhcp/dhcp.go` | paired v4/v6 allocation over two bitmaps |
| `pkg/handler/network.go` | `localTunIPv4`/`localTunIPv6`, `ChangeTunIP` |
| `pkg/tun/ip_*.go`, `pkg/tun/iptables_linux.go` | per-OS v6 address + ip6tables |
| `pkg/core/gvisor_stack.go` | dual network protocol registration + forwarding |
| `pkg/dns/dns_windows.go` | `SetDNS(AF_INET6)` |
| `pkg/inject/envoy.go`, `container.go`, `fargate.go` | v6 rule propagation + template selection |
| `pkg/util/net_others.go`, `net_windows.go`, `net.go` | `IsIPv6Enabled`, `IsIPv6`, ICMPv6 |

## 7. Related Docs

- [03-dhcp-ip-allocation.md](03-dhcp-ip-allocation.md) — paired DHCP allocation
- [22-tun-device.md](22-tun-device.md) — per-OS TUN v6 assignment
- [16-envoy-controlplane.md](16-envoy-controlplane.md) — per-family xDS listeners
- [17-sidecar-injection.md](17-sidecar-injection.md) — envoy template selection
- [19-dns-resolution.md](19-dns-resolution.md) — DNS for both families
- [18-gvisor-network-stack.md](18-gvisor-network-stack.md) — dual-protocol stack
- [09-tun-ip-hot-update.md](09-tun-ip-hot-update.md) — hot-update of both addresses
