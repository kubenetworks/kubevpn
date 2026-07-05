# DNS Resolution Design

## 1. Overview

The DNS package (`pkg/dns`) configures the local machine's DNS to resolve Kubernetes service names when connected via KubeVPN. It has two main components: a DNS forward server that runs in the traffic manager pod, and platform-specific client-side DNS setup that configures the local machine to use the cluster's DNS.

> **Service records source:** the `/etc/hosts` short-name entries (and macOS resolver files)
> are no longer driven by a client-side service informer. The traffic manager discovers
> services and pushes them to the client, which feeds them to `dns.Config.UpdateServices`
> (see [44-server-side-route-discovery.md](44-server-side-route-discovery.md)). Hosts remain
> add-only. If server discovery is unavailable, short-name hosts entries are simply absent
> (name resolution still works via the cluster DNS forward server + search domains).

## 2. Architecture

```
Local Machine                          Traffic Manager Pod
┌──────────────────────────┐           ┌─────────────────────────────┐
│  App resolves "my-svc"   │           │  DNS Forward Server (:53)   │
│         │                │           │  ├── LRU cache (10k entries) │
│         ▼                │           │  ├── search domain expansion │
│  TUN Device DNS config   │ ─────────►│  └── fan-out to upstream DNS │
│  (systemd-resolved /     │           │         │                    │
│   /etc/resolver /        │           │         ▼                    │
│   LUID.SetDNS)           │           │  kube-dns / CoreDNS          │
│         +                │           └─────────────────────────────┘
│  /etc/hosts entries      │
│  (service.name → ClusterIP)
└──────────────────────────┘
```

## 3. DNS Forward Server (`forward_server.go`)

Runs in the traffic manager pod as the `dns` container (`kubevpn dns`), listening on port 53.

### Resolution Strategy

For each query (e.g., `my-svc`):

1. **Answer cache check**: an LRU cache (10k entries) keyed by `(name, qtype, qclass)` stores the
   **full response** (positive or negative). On a hit the cached response is served directly — the
   query's ID is stamped in and record TTLs are decremented by the time spent in cache — with **zero
   upstream queries**.
2. **Single-flight**: concurrent identical misses are collapsed (`golang.org/x/sync/singleflight`)
   into a single upstream resolution shared by all waiters, avoiding a thundering herd.
3. **Search domain expansion + fan-out**: expand the name with all search domains (bare +
   `my-svc.default.svc.cluster.local.` + `my-svc.svc.cluster.local.` + `my-svc.cluster.local.`) and
   send `name × upstream server` queries **concurrently**. The **first** answer wins (atomic
   `CompareAndSwap`) and the remaining in-flight branches are **cancelled** (ctx cancel) so a slow /
   NXDOMAIN branch never delays the response or lingers to the 5s timeout.
4. **Cache update**: cache the winning response — positive entries expire at the answer's **minimum
   record TTL** (capped at 30 min); a real NXDOMAIN/NODATA is **negatively cached** for the authority
   SOA minimum (capped at 30s). A transient all-upstreams-failed result returns SERVFAIL and is **not**
   cached (so the next query retries).
5. **Response rewrite**: answer/question names are rewritten from the expanded name back to the
   original queried name before caching, so cached responses are ready to serve.

This makes the forward server a proper caching resolver: repeat lookups (the common `curl my-svc`
pattern) never touch CoreDNS, and search-domain misses are absorbed by the negative cache.

## 4. Client-Side DNS Setup

### 4.1 Linux (`dns_linux.go`)

**Split DNS only — the cluster nameserver is always scoped to cluster domains on the TUN
interface, never written to the global `/etc/resolv.conf`.** Two tiers:

1. **systemd-resolved** (preferred, via CLI):
   - `resolvectl dns <tun> <dnsIP>` — set DNS server on the TUN interface
   - `resolvectl domain <tun> <search domains>` — scope those domains to the TUN (no `~.`, so
     it is a split resolver, not a global default route)
   - Falls back to `systemd-resolve --set-dns` for older systems
2. **Library DNS** (tailscale `dns.OSConfigurator`) — used only if `SupportsSplitDNS()`:
   - Covers systemd-resolved (D-Bus), NetworkManager, openresolv and debian-resolvconf
   - `buildLibraryOSConfig` sets **`MatchDomains` = cluster search domains**. This is what makes
     the library configure a *split* resolver: an **empty** `MatchDomains` would trip
     `SetLinkDefaultRoute` in tailscale's `resolved.go` and hijack **all** host DNS through the
     TUN. Non-empty routes only cluster domains there.

There is **no `/etc/resolv.conf` fallback**. On a host with no split-capable DNS manager
(`SupportsSplitDNS()` false → `errNotSupportSplitDNS`), setup logs a warning and returns
without touching the global resolver; cluster **service names still resolve via `/etc/hosts`**
entries pushed by the traffic manager, while other cluster FQDNs (e.g. raw pod DNS) do not
resolve on such hosts. This is a deliberate trade-off: never pollute the global resolver.

**Cleanup** (`CancelDNS`): closes the OS configurator (reverts the library split config) and
removes hosts file entries. The resolvectl/systemd-resolve per-interface settings are dropped
by systemd-resolved when the TUN device is destroyed; nothing was written to
`/etc/resolv.conf`, so there is nothing to undo there.

### 4.2 macOS (`dns_unix.go`)

Uses the `/etc/resolver/` directory for split DNS:

- Creates resolver files (e.g., `/etc/resolver/svc.cluster.local`) pointing to the cluster DNS
- Watches K8s services via informer and creates per-service resolver files for short-name resolution
- Filters out common TLDs (`com`, `io`, `net`, `org`, `cn`, `ru`) to avoid hijacking real domains

### 4.3 Windows (`dns_windows.go`)

Uses Windows LUID APIs via `winipcfg`:

- `luid.SetDNS(AF_INET, servers, searchDomains)` — sets DNS servers on TUN interface for IPv4
- `luid.SetDNS(AF_INET6, servers, searchDomains)` — same for IPv6
- Cleanup flushes DNS and routes from the TUN interface

## 5. Hosts File Management (`dns.go`)

For service short-name resolution (e.g., `curl my-svc:8080`), KubeVPN adds entries to `/etc/hosts`:

```
10.96.0.1    my-svc    # kubevpn-tun0
10.96.0.2    other-svc # kubevpn-tun0
```

### Entry Management

- **Generation**: `generateAppendHosts()` maps each K8s Service ClusterIP to its short name, deduplicating against existing hosts entries in O(n) via a single reused membership set (not a per-entry set rebuild)
- **Watching**: Service informer watches for adds/updates/deletes, debouncing at `DNSRouteDebounceInterval` then refreshing at `DNSRouteRefreshInterval`
- **Cleanup**: `CleanupHosts()` removes all lines containing the KubeVPN marker keyword

### Hosts File Locking

`withHostsFileLock()` uses `flock(LOCK_EX)` on Unix (`dns_lock_unix.go`) or a global named Mutex on Windows (`dns_lock_windows.go`, via `windows.CreateMutex`/`WaitForSingleObject`) to prevent concurrent writes from multiple KubeVPN instances. If the lock cannot be acquired, `fn` runs unlocked as a best-effort fallback (single-instance usage stays correct) on both platforms.

## 6. Related Files

| File | Purpose |
|------|---------|
| `pkg/dns/forward_server.go` | DNS forward server (traffic manager pod) |
| `pkg/dns/dns.go` | Hosts file management, Config struct |
| `pkg/dns/dns_linux.go` | Linux DNS setup (split DNS: systemd-resolved / tailscale library) |
| `pkg/dns/dns_unix.go` | macOS DNS setup (/etc/resolver) |
| `pkg/dns/dns_windows.go` | Windows DNS setup (LUID API) |
| `pkg/dns/dns_lock_unix.go` | Unix hosts file locking (flock) |
| `pkg/dns/dns_lock_windows.go` | Windows hosts file locking (Mutex) |
