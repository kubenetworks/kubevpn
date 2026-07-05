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

**Read-error resilience:** The core read loop that drives `tunConn.Read` (`pkg/core` `tunDevice.pumpTun`) tolerates transient read errors instead of tearing down the data plane. On a read error: if the context is cancelled it returns cleanly (normal shutdown, no noise); otherwise it logs a warning, backs off briefly (`tunReadErrorBackoff`, 100ms) and retries. Only after `maxConsecutiveTunReadErrors` (10) consecutive failures is the device declared dead (reported on `errChan`, which tears down the device and connection pool). A single successful read resets the counter. This keeps a one-off, recoverable error from killing the tunnel while still detecting a genuinely broken device. See §5.2 for the macOS case that motivated it.

## 5. Platform-Specific Implementation

### 5.1 Linux (`tun_linux.go`)

- **Device creation**: Scans existing interfaces for `utunN` names, calculates `maxIndex+1`, then calls `tun.CreateTUN(fmt.Sprintf("utun%d", maxIndex+1), mtu)` for explicit naming
- **IP assignment**: `netlink.NetworkLinkAddIp(ifce, ipv4, ipNet)` — uses netlink directly
- **Route addition**: `netlink.AddRoute(route.String(), "", "", tunName)` via libcontainer/netlink
- **Interface up**: `netlink.NetworkLinkUp(ifce)`

### 5.2 macOS (`tun_darwin.go`)

- **Device creation**: `tun.CreateTUN("utun", mtu)` — macOS kernel assigns utunN names
- **IP assignment**: `unix.SIOCSIFADDR_IN6` ioctl for IPv6; `ifconfig` command for IPv4
- **Route addition**: `exec.Command("route", "add", "-net", cidr, "-interface", tunName)`
- **Interface up**: Automatic with IP assignment on macOS
- **Route-listener `ENOBUFS` (resilience):** wireguard-go runs a route-socket listener goroutine for interface up/down/MTU events. On an `RTM_IFINFO` message it calls `net.InterfaceByIndex` → `route.FetchRIB`, which under heavy routing-table churn (adding pod/service/CIDR routes during connect) can return `ENOBUFS` — surfaced as `route ip+net: no buffer space available`. wireguard retries only `ENOMEM`, so on `ENOBUFS` the listener exits and delivers the error **once** through the next `Read`. The utun fd itself stays usable, and kubevpn never consumes wireguard `Events()`, so the loss of the listener is harmless. The core read loop therefore tolerates this one-off error (§4) and keeps the data plane alive instead of tearing it down. Previously this manifested as `[TUN] device exited: route ip+net: no buffer space available` followed by the connection pool being cancelled, while `connect` still reported success.

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

## 9. Windows Driver Installation & Embedding (`pkg/driver`)

On Linux/macOS the kernel provides the TUN device, but on **Windows** the driver must be installed
first. `pkg/driver` embeds the driver binaries into the kubevpn executable and drops/installs them
at runtime. There are two drivers:

### 9.1 Wintun (WireGuard userspace TUN)

This is the driver `tun_windows.go` actually uses. `wintun.dll` is embedded **per architecture**
via build-tagged files, each with its own `//go:embed`:

| File | Build tag | Embeds |
|---|---|---|
| `wintun/amd64.go` | `windows && amd64` | `bin/amd64/wintun.dll` |
| `wintun/arm64.go` | `windows && arm64` | `bin/arm64/wintun.dll` |
| `wintun/arm.go` | `windows && arm` | `bin/arm/wintun.dll` |
| `wintun/x86.go` | `windows && x86` | `bin/x86/wintun.dll` |
| `wintun/others.go` | `!windows` | stub returning "not implement" |

Each arch's `InstallWintunDriver()` reads its embedded DLL and calls the shared `copyDriver`
(`wintun/func.go`), which writes `wintun.dll` **next to the executable** — but only if it is
missing or its bytes differ (content-compared to avoid needless rewrites/locking).

`driver.InstallWireGuardTunDriver()` wraps this in `retry.OnError` (default backoff) and is called
from `pkg/handler/network.go` before TUN creation. `UninstallWireGuardTunDriver()` simply removes
the `wintun.dll` beside the executable (called from `pkg/util/util.go` cleanup).

### 9.2 TAP-Windows (OpenVPN, legacy fallback)

`openvpn/windows.go` embeds the `tap-windows-9.21.2.exe` installer (`//go:embed
exe/tap-windows-9.21.2.exe`). `Install()` writes it to a temp `.exe`, chmods it, and runs it with
`/S` (silent). `driver.installTunTapDriver()` retries this. Uninstall
(`driver.uninstallTunTapDriver`) scans drive letters via `getDiskName()` and runs
`<drive>:\Program Files\TAP-Windows\Uninstall.exe /S`.

## 10. Related Files

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
| `pkg/driver/driver.go` | Install/uninstall orchestration (retry, drive-letter scan) |
| `pkg/driver/wintun/{amd64,arm64,arm,x86,others}.go` | Per-arch embedded `wintun.dll` + `InstallWintunDriver` |
| `pkg/driver/wintun/func.go` | `copyDriver` (content-compared write next to exe) |
| `pkg/driver/openvpn/windows.go` | Embedded TAP-Windows installer `Install()` |
