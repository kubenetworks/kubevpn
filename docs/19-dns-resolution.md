# DNS Resolution Design

## 1. Overview

The DNS package (`pkg/dns`) configures the local machine's DNS to resolve Kubernetes service names when connected via KubeVPN. It has two main components: a DNS forward server that runs in the traffic manager pod, and platform-specific client-side DNS setup that configures the local machine to use the cluster's DNS.

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

1. **Cache check**: LRU cache (10k entries, 30-min TTL) maps original names to resolved FQDN
2. **Search domain expansion**: If not cached, expand the name with all search domains:
   - `my-svc.` (bare name)
   - `my-svc.default.svc.cluster.local.`
   - `my-svc.svc.cluster.local.`
   - `my-svc.cluster.local.`
3. **Fan-out resolution**: For each expanded name × each upstream DNS server, send queries **concurrently**. First successful response wins (atomic `CompareAndSwap`).
4. **Cache update**: Store the winning name expansion for future lookups
5. **Response rewrite**: Replace expanded name back to original in answer records

This fan-out approach minimizes latency — the first upstream server to respond wins.

## 4. Client-Side DNS Setup

### 4.1 Linux (`dns_linux.go`)

Three-tier fallback strategy:

1. **systemd-resolved** (preferred):
   - `resolvectl dns <tun> <dnsIP>` — set DNS server on TUN interface
   - `resolvectl domain <tun> <search domains>` — set search domains
   - Falls back to `systemd-resolve --set-dns` for older systems
2. **Library DNS** (tailscale `dns.OSConfigurator`):
   - Uses tailscale's OS-specific DNS configurator
   - Only used if it supports split DNS
3. **`/etc/resolv.conf`** (fallback):
   - Prepends cluster DNS server to existing nameserver list
   - Uses Docker's `resolvconf.Build()` to write

**Cleanup** (`CancelDNS`): Closes the OS configurator, removes the added DNS server from resolv.conf, and removes hosts file entries.

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

- **Generation**: `generateAppendHosts()` maps each K8s Service ClusterIP to its short name, deduplicating against existing hosts entries
- **Watching**: Service informer watches for adds/updates/deletes, debouncing at `DNSRouteDebounceInterval` then refreshing at `DNSRouteRefreshInterval`
- **Cleanup**: `CleanupHosts()` removes all lines containing the KubeVPN marker keyword

### Hosts File Locking

`withHostsFileLock()` uses `flock(LOCK_EX)` on Unix or a named Mutex on Windows to prevent concurrent writes from multiple KubeVPN instances.

## 6. Related Files

| File | Purpose |
|------|---------|
| `pkg/dns/forward_server.go` | DNS forward server (traffic manager pod) |
| `pkg/dns/dns.go` | Hosts file management, Config struct |
| `pkg/dns/dns_linux.go` | Linux DNS setup (systemd-resolved / resolv.conf) |
| `pkg/dns/dns_unix.go` | macOS DNS setup (/etc/resolver) |
| `pkg/dns/dns_windows.go` | Windows DNS setup (LUID API) |
| `pkg/dns/dns_lock_unix.go` | Unix hosts file locking (flock) |
| `pkg/dns/dns_lock_windows.go` | Windows hosts file locking (Mutex) |
