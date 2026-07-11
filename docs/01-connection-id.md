# ConnectionID Design Document

## Overview

ConnectionID is a string identifier that uniquely identifies a VPN connection to a specific Kubernetes cluster namespace.
It is derived from the Kubernetes namespace UID and serves as the primary key for managing multiple concurrent VPN
connections in the daemon process.

## Generation

ConnectionID is the **last 12 hex characters of the target namespace's UID**.

```go
// pkg/util/kubeconfig.go
func GetConnectionID(ctx context.Context, client v1.NamespaceInterface, ns string) (string, error) {
    namespace, err := client.Get(ctx, ns, metav1.GetOptions{})
    if err != nil {
        return "", err
    }
    return string(namespace.UID[len(namespace.UID)-12:]), nil
}
```

**Why namespace UID?** Each VPN connection targets a specific namespace where the traffic-manager is deployed. The
namespace UID is immutable for the lifetime of the namespace, globally unique within the cluster, and does not require
any additional state management. Using the last 12 hex characters (48 bits) provides sufficient uniqueness while keeping
the ID short enough for display in CLI output.

**Collision risk:** 48-bit space means ~280 trillion possible values. For the typical use case (a single user with < 10
concurrent connections), collision probability is negligible.

**Namespace recreation:** If a namespace is deleted and recreated, it gets a new UID, which means a new ConnectionID.
This is correct behavior — the old traffic-manager and DHCP state are gone, so the old connection is invalid.

## Lifecycle

```
1. Connect request arrives at daemon
2. InitClient() → resolves kubeconfig, creates K8s clientset
3. InitDHCP() → creates/gets DHCP ConfigMap, calls GetConnectionID()
   └── connectionID cached in dhcp.Manager.connectionID
4. Dedup check: findConnection(connectionID)
   ├── Found → reuse existing connection, set as current
   └── Not found → proceed with new connection
5. Connection established → appended to svr.connections[]
6. connectionID set as svr.currentConnectionID
7. connectionID returned to CLI client via gRPC ConnectResponse
```

## Storage

### Server-side (daemon process)

| Location | Type | Purpose |
|---|---|---|
| `dhcp.Manager.connectionID` | `string` | Cached at DHCP init, exposed via `GetConnectionID()` |
| `handler.ConnectOptions` | (delegates to dhcp) | `GetConnectionID()` → `dhcp.GetConnectionID()` |
| `action.Server.connections` | `[]*ConnectOptions` | All active connections (ordered slice) |
| `action.Server.currentConnectionID` | `string` | The "active" connection for implicit commands |

### Client-side (CLI process)

| Location | Purpose |
|---|---|
| `ConnectResponse.connectionID` | Received from daemon after connect |
| `proxy_out_manager` state files | `<stateDir>/<connectionID>.json` — tracks managed SOCKS proxy processes |
| `StatusRequest.ConnectionIDs` | Filter for status queries |
| `DisconnectRequest.ConnectionID` | Target for disconnect |

## Usage by RPC

| RPC | How connectionID is used |
|---|---|
| **Connect** | Generated after `InitDHCP()`. Used to dedup (skip if already connected). Returned to client. Set as `currentConnectionID`. |
| **Disconnect** | Three modes: by connectionID (exact match), by kubeconfig (resolves to connectionID), or all. After removal, `resetCurrentConnection()` picks first remaining. |
| **Proxy** | Internally calls Connect, then uses the returned connectionID to find the `ConnectOptions` for injecting inbound proxy. |
| **Leave** | Uses `currentConnectionID` to find connection, then removes proxy from workloads. |
| **Sync** | Extracts connectionID from Connect stream, uses it to attach `SyncOptions` to the connection. |
| **Unsync** | Uses `currentConnectionID` to find connection, then cleans up sync. |
| **Route** | Uses `currentConnectionID` to find TUN device name for route manipulation. |
| **Status** | If `ConnectionIDs` provided, filters by them. Otherwise returns all. Includes connectionID in each `Status`, `Proxy`, and `Sync` response. |
| **ConnectionList** | Returns all connections with their connectionIDs and which is current. |
| **ConnectionUse** | Switches `currentConnectionID` to the specified ID. |

## Key Helpers

| Method | File | Purpose |
|---|---|---|
| `findConnection(id)` | `connection.go` | O(n) lookup by connectionID, returns `(*ConnectOptions, index)` |
| `removeConnection(id)` | `connection.go` | Removes all matching connections from slice, returns them for cleanup |
| `resetCurrentConnection(id)` | `connection.go` | After removing a connection, picks first remaining as current (or clears) |

## Design Decisions

### Why a slice, not a map?

`Server.connections` is `[]*ConnectOptions` (slice) rather than `map[string]*ConnectOptions`. Reasons:

1. **Connection count is tiny** — users rarely have more than 2-3 concurrent connections. O(n) scan is negligible.
2. **Order matters** — `resetCurrentConnection` picks the first remaining connection. Sorted cleanup in disconnect uses
   `Connects.Sort()`.
3. **Simplicity** — no need to maintain a parallel ordered-key list alongside a map.

### Why currentConnectionID?

Many commands (leave, unsync, route) operate on "the current connection" without requiring the user to specify which one.
This is a UX convenience — similar to `kubectl config use-context`. The user can switch with `kubevpn connection use`.

### Why 12 characters?

12 hex characters = 48 bits. Short enough for CLI display, long enough to be practically unique. Kubernetes UIDs are
UUIDv4 (128 bits), so any 12-character suffix has the same entropy as a random 48-bit value.
