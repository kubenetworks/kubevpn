# TUN Device Management Design

## 1. Overview

The tun package (`pkg/tun`) manages TUN virtual network interfaces across Linux, macOS, and Windows. It handles device creation, IP assignment, route management, and iptables rules. The package uses the `wireguard/tun` library for cross-platform TUN device creation.

## 2. Architecture

```
tun.Listener(Config)
  │
  ├── createTun(Config)           ← platform-specific
  │   ├── tun.CreateTUN(name, mtu)   ← wireguard/tun library
  │   ├── setIP(ifName, ipv4, ipv6)  ← platform-specific
  │   ├── addTunRoutes(ifName, routes)
  │   └── setMTU / interface up
  │
  └── returns net.Listener
      └── Accept() → tunConn (implements net.Conn)
```

## 3. Config

```go
type Config struct {
    Name    string        // device name (auto-generated if empty)
    Addr    string        // IPv4 CIDR (e.g. "198.18.0.5/16")
    Addr6   string        // IPv6 CIDR (e.g. "2001:2::5/64")
    MTU     int           // defaults to config.DefaultMTU
    Routes  []types.Route // CIDRs to route through TUN
    Gateway string        // gateway IP for routes
}
```

## 4. tunConn — net.Conn Adapter

`tunConn` wraps a `wireguard/tun.Device` as `net.Conn`:
- **Read**: Reads from TUN device at `MessageTransportHeaderSize` offset (wireguard internal header), copies payload to caller's buffer
- **Write**: Copies data after the transport header offset, writes to TUN device
- **Deadlines**: Not supported (returns OpError)
- **LocalAddr**: Returns the TUN device's IP address

## 5. Platform-Specific Implementation

### 5.1 Linux (`tun_linux.go`)

- **Device creation**: `tun.CreateTUN("utun", mtu)` — names are auto-assigned (utun0, utun1, ...)
- **IP assignment**: `netlink.NetworkLinkAddIp(ifce, ipv4, ipNet)` — uses netlink directly
- **Route addition**: `netlink.AddRoute(route.String(), "", "", tunName)` via libcontainer/netlink
- **Interface up**: `netlink.NetworkLinkUp(ifce)`

### 5.2 macOS (`tun_darwin.go`)

- **Device creation**: `tun.CreateTUN("utun", mtu)` — macOS kernel assigns utunN names
- **IP assignment**: `unix.SIOCSIFADDR_IN6` ioctl for IPv6; `ifconfig` command for IPv4
- **Route addition**: `exec.Command("route", "add", "-net", cidr, "-interface", tunName)`
- **Interface up**: Automatic with IP assignment on macOS

### 5.3 Windows (`tun_windows.go`)

- **Device creation**: Uses wintun driver via `wireguard/tun.CreateTUNWithRequestedGUID`
- **IP assignment**: `winipcfg.LUID.SetIPAddresses()` via Windows IP Helper API
- **Route addition**: `winipcfg.LUID.AddRoute()` with metric configuration
- **DNS integration**: Uses `winipcfg.LUID.SetDNS()` for Windows DNS

## 6. Route Management (`route.go`)

Platform-independent API:

```go
func AddRoutes(tunName string, routes ...types.Route) error
func DeleteRoutes(tunName string, routes ...types.Route) error
```

Routes are the cluster CIDRs (pod CIDR, service CIDR) that should be routed through the TUN device. Each platform implements `addTunRoutes` / `deleteTunRoutes`.

## 7. IP Hot-Update (`ip.go`)

```go
func ChangeIP(ifName string, oldAddr, newAddr string) error
```

Replaces the IP address on an existing TUN device without destroying it. The TUN file descriptor remains valid — only the OS interface metadata changes. Used by the TUN IP hot-update mechanism (see `09-tun-ip-hot-update.md`).

## 8. iptables Rules (`iptables_linux.go`)

On Linux, `UpdateDNAT(oldIP, newIP)` updates iptables DNAT rules when the TUN IP changes:
- Removes old DNAT rule pointing to `oldIP`
- Adds new DNAT rule pointing to `newIP`

Non-Linux platforms have a no-op implementation (`iptables_others.go`).

## 9. Related Files

| File | Purpose |
|------|---------|
| `pkg/tun/tun.go` | Config, Listener, tunConn adapter |
| `pkg/tun/tun_linux.go` | Linux TUN creation (netlink) |
| `pkg/tun/tun_darwin.go` | macOS TUN creation (ioctl + route cmd) |
| `pkg/tun/tun_windows.go` | Windows TUN creation (wintun + winipcfg) |
| `pkg/tun/ip.go` | ChangeIP (hot-update) |
| `pkg/tun/ip_linux.go` | Linux IP change (netlink) |
| `pkg/tun/ip_darwin.go` | macOS IP change (ifconfig) |
| `pkg/tun/ip_windows.go` | Windows IP change (LUID API) |
| `pkg/tun/route.go` | AddRoutes / DeleteRoutes |
| `pkg/tun/route_linux.go` | Linux route management (netlink) |
| `pkg/tun/route_darwin.go` | macOS route management (route cmd) |
| `pkg/tun/route_windows.go` | Windows route management (LUID) |
| `pkg/tun/iptables_linux.go` | iptables DNAT rules |
| `pkg/tun/iptables_others.go` | No-op for non-Linux |
