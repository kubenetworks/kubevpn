# TUN Device Management Design

## 1. Overview

The tun package (`pkg/tun`) manages TUN virtual network interfaces across Linux, macOS, and Windows. It handles device creation, IP assignment, route management, and iptables rules. The package uses the `wireguard/tun` library for cross-platform TUN device creation.

## 2. Architecture

```
tun.Listener(Config)
  │
  ├── createTun(Config)           ← platform-specific
  │   ├── tun.CreateTUN(name, mtu)   ← wireguard/tun library
  │   ├── newBatchDevice(device)     ← immediately start Events() drainer
  │   ├── setIP(ifName, ipv4, ipv6)  ← platform-specific
  │   ├── addTunRoutes(ifName, routes)
  │   └── setMTU / interface up
  │
  └── returns net.Listener
      └── Accept() → tunConn (implements net.Conn)
```

### 2.1 Event consumer lifecycle

Every platform constructs `newBatchDevice` immediately after native TUN creation,
before name lookup, IP/MTU configuration, routes or interface-up operations. Its single
consumer drains `Events()` so startup event producers cannot block on an unread channel.
This is **drain-only**: events do not implement dynamic MTU updates or reconnect policy.

On any subsequent initialization failure, the deferred adapter cleanup closes the device.
`batchDevice.Close` uses `sync.Once`: it calls native `Close` **while the consumer is still
running**, then joins the consumer after the native event channel closes, preserving the
native close result. Stopping the consumer first could block native shutdown. A nil event
channel needs no consumer. Windows additionally guards its wrapper close/uninstall/library
cleanup with `sync.Once`, preventing repeated wrapper cleanup on concurrent closes.

## 3. Config

```go
type Config struct {
    Name    string        // device name (auto-generated if empty)
    Addr    string        // IPv4 CIDR (e.g. "198.18.0.5/32")
    Addr6   string        // IPv6 CIDR (e.g. "2001:2::5/128")
    MTU     int           // defaults to config.DefaultMTU
    Routes  []types.Route // CIDRs to route through TUN
    Gateway string        // gateway IP for routes
}
```

## 4. tunConn — net.Conn Adapter + Batched IO

`tunConn` (and `winTunConn` on Windows) wraps a `wireguard/tun.Device`. Since wireguard-go
v0.0.20230223 the device API is **batched and vectorized** (to support GSO/GRO offload):

```go
Read(bufs [][]byte, sizes []int, offset int) (n int, err error)
Write(bufs [][]byte, offset int) (int, error)
BatchSize() int
```

The shared adapter logic lives in `pkg/tun/batch.go` (`batchDevice`), embedded by both
`tunConn` and `winTunConn`. It provides:

- **`net.Conn` compatibility** (`Read`/`Write`) so `Listener.Accept()` keeps returning a
  `net.Conn`. Write copies one IP packet into scratch with the required header room and
  issues a single-packet batched write. Read hands out one packet per call, refilling from a
  batched read — this is only the non-batch fallback (e.g. `net.Pipe` in tests).
- **`ReadPackets(bufs, sizes, off)`** — the segment-aware read hot path used by `pkg/core`
  `tunDevice.pumpTunBatch`. One read returns a compact valid prefix at
  `bufs[i][off:off+sizes[i]]`, with `off` = the data plane's `tunReserve` (3).
  Qualified Linux native reads fill these buffers directly; other reads copy from scratch.
- **`WritePackets(packets)`** — copies into private scratch before native writes, preserving
  caller-owned packets even when Linux GRO mutates/coalesces them.
- **`BatchSize()`** — max packets per read/write.

### 4.1 Offset bridge (wireguard headroom vs data-plane layout)

Read and write have different ownership requirements. Vendored Linux `NativeTun.Read`
uses `handleVirtioRead` / `gsoSplit` to split kernel GSO input into caller-provided buffers;
it does **not** GRO-append/reallocate their bases. Direct read is therefore restricted to
Linux `*tun.NativeTun` with every destination having capacity for `off + MaxSegmentSize`.
Neither MTU nor `BatchSize()` alone qualifies a device.

Linux `config.LPool` keeps logical length **65536**, with backing capacity **65538**.
The adapter passes temporary aliases of length **65538** to native Read at offset **3**,
allowing a full **65535-byte** segment to be received before applying the logical frame
limit (§4.3). Filtering compacts valid packets by swapping whole logical buffer slices,
not copying payloads; each original base remains owned exactly once, including dropped
siblings. Temporary aliases are cleared after the read. The `[2 len][1 type][IP]` layout
and wire header are unchanged.

Non-Linux, non-native devices and insufficient-capacity destinations retain the full-size
scratch-and-copy fallback. Native offload reads still have wireguard-go's internal read /
split work; direct read removes the **adapter's** extra copy, not all kernel-to-packet copies.
Writes still copy into private scratch at `tunOffset = MessageTransportOffsetContent` (16)
and clear those 16 headroom bytes before each write. This preserves caller ownership across
Linux GRO mutation/coalescing and supplies the virtio-net header room; it is unchanged by
the read optimization.

### 4.2 GRO/GSO offload is Linux-only

| Platform | `BatchSize()` | Offload (GRO/GSO) | One `Read` yields |
|---|---|---|---|
| Linux | `IdealBatchSize` (128) when `TUNSETOFFLOAD` succeeds, else `1` | Yes (`offload_linux.go`) | up to 128 packets after GSO splitting |
| macOS | `1` (hardcoded) | No | 1 packet |
| Windows | `1` (hardcoded) | No | 1 packet |

Linux offload can amortize read syscalls across split packets and coalesce writes with GRO.
Each returned read packet is still framed and dispatched separately. macOS/Windows use the
same pump ownership rules with one packet per read and the scratch fallback. Restricted
Linux can report `BatchSize()==1` yet still qualify for direct reads by native type and
capacity. These are mechanism-level benefits, not measured end-to-end throughput claims.

### 4.3 Frame-boundary guard

The tunnel frames each packet as `[2-byte length][1-byte type][IP]` into a logically
65536-byte buffer, so the maximum IP length is **65533**, not the backing capacity minus
headroom. `ReadPackets` receives native segments up to 65535 bytes safely, then drops those
exceeding the logical limit with a warning; TCP can retransmit. Extra capacity does not
increase the frame limit. `ErrTooManySegments` is non-fatal: only the native API's reported
prefix is considered, valid packets are compacted, and the error is swallowed.

### 4.4 Read-error resilience

The core read loop (`pkg/core` `tunDevice.pumpTun`, which dispatches to `pumpTunBatch` for
batched devices or `pumpTunSingle` for plain `net.Conn`) tolerates transient read errors
instead of tearing down the data plane. On a read error: if the context is cancelled it
returns cleanly (normal shutdown, no noise); otherwise it logs a warning, backs off briefly
(`tunReadErrorBackoff`, 100ms) and retries. Only after `maxConsecutiveTunReadErrors` (10)
consecutive failures is the device declared dead (reported on `errChan`, which tears down the
device and connection pool). A single successful read resets the counter. This keeps a
one-off, recoverable error from killing the tunnel while still detecting a genuinely broken
device. See §5.2 for the macOS case that motivated it.

The batched pump dispatches a valid reported prefix **before** handling an accompanying
read error. Unused, invalid and malformed buffer slots stay pump-owned for the next read;
only buffers transferred to dispatch are replaced from the pool. Cancellation takes
precedence over further dispatch, interrupts the batch retry wait, and returns every
remaining pump-owned buffer exactly once. Dispatched buffers remain consumer-owned.

### 4.5 Memory trade-off (host allocation model)

The Darwin-host allocation model in `pkg/core/memory_bench_test.go` measures explicit
capacities representing the Linux design; Darwin's actual `LPool` remains length/capacity
65536. A requested 65538-byte backing allocation rounds to **73728 bytes (72 KiB)**,
**8192 bytes more per distinct Linux pooled buffer** than before. Removing the adapter's
128 read-scratch buffers (65551 requested bytes each, also rounded to 73728) removes
**9 MiB** of allocated backing, not just their logical byte total.

With 128 pump-owned buffers plus distinct queued buffers, the modeled net change is:

| Distinct queued buffers | Added pooled backing | Removed read scratch | Net change |
|---|---|---|---|
| 256 | 3 MiB | 9 MiB | -6 MiB |
| 2048 | 17 MiB | 9 MiB | +8 MiB |

Shared packet references count only once. Other Linux `LPool` users also pay the larger
allocation cost; fallback read scratch and write scratch are not removed. This is not a
global memory-reduction claim, process RSS measurement, or real Linux-kernel benchmark.

- **Deadlines**: Not supported (returns OpError)
- **LocalAddr**: Returns the TUN device's IP address

## 5. Platform-Specific Implementation

### 5.1 Linux (`tun_linux.go`)

- **Device creation**: Scans existing interfaces for `utunN` names, calculates `maxIndex+1`, then calls `tun.CreateTUN(fmt.Sprintf("utun%d", maxIndex+1), mtu)` for explicit naming
- **IP assignment**: `netlink.NetworkLinkAddIp(ifce, ipv4, ipNet)` — uses netlink directly
- **Route addition**: `netlink.AddRoute(route.String(), "", "", tunName)` via libcontainer/netlink
- **Interface up**: `netlink.NetworkLinkUp(ifce)`
- **Offload**: wireguard-go negotiates `TUNSETOFFLOAD` (TCP/UDP GRO/GSO); on success `BatchSize()` becomes 128. Native reads split GSO input into caller buffers; writes may GRO-coalesce packets (see §4.2).

### 5.2 macOS (`tun_darwin.go`)

- **Device creation**: `tun.CreateTUN("utun", mtu)` — macOS kernel assigns utunN names
- **IP assignment**: `unix.SIOCSIFADDR_IN6` ioctl for IPv6; `ifconfig` command for IPv4
- **Route addition**: `exec.Command("route", "add", "-net", cidr, "-interface", tunName)`
- **Interface up**: Automatic with IP assignment on macOS
- **Historical `ENOBUFS` incident:** During routing-table churn, the older route-listener path through `net.InterfaceByIndex` → `route.FetchRIB` surfaced `route ip+net: no buffer space available`. Its `ENOMEM`-only retry behavior did not handle that error. The one-off error then caused `[TUN] device exited: route ip+net: no buffer space available` and connection-pool cancellation while `connect` still reported success. This motivated the core read-error tolerance (§4.4); it does **not** describe the pinned listener implementation.
- **Current route-socket listener:** In vendored `wireguard/tun/tun_darwin.go`, `routineRouteListener` reads interface flags and MTU directly from matching `RTM_IFINFO` messages, without `InterfaceByIndex` / `FetchRIB` on that path. It retries `EINTR` from `unix.Read`; other read errors are sent once to the native error channel, then the listener exits and closes `Events()`. Native TUN `Read` surfaces that queued error once, and the adapter's event consumer ends when the event channel closes. Listener exit alone does not close the utun fd: the core can retry packet reads, while the underlying transport and heartbeat monitoring continue. Event notifications are no longer available, so this is not a blanket claim that event loss is harmless or that the data path remains healthy; packet I/O and correlated heartbeat evidence still determine that.

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
| `pkg/tun/batch.go` | `batchDevice` — batched/offload TUN IO, offset bridge, frame-boundary guard, net.Conn shims |
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
