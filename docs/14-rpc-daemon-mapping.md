# RPC Method → Daemon Mapping

## Overview

Both user daemon and root daemon register the same `Server` struct with all RPC methods. The `IsSudo` flag distinguishes which daemon is running. Some RPCs run in both daemons (branching on `IsSudo`), some only make sense in one.

## Method Matrix

| RPC Method | CLI calls | Runs in | IsSudo check | Forwards to | Notes |
|---|---|---|---|---|---|
| `Connect` | user daemon | **both** | ✅ line 18,38 | user→sudo (via `redirectConnectToSudoDaemon`) | User: control plane (traffic mgr, proxy inject). Sudo: data plane (TUN, DHCP, DNS) |
| `Disconnect` | user daemon | **both** | ✅ line 20,45 | user→sudo (forwards first, then cleans up user side) | User: disconnect sudo first, then clean up SSH/connections |
| `Proxy` | user daemon | **user only** | ❌ | user→user self-call (`Connect`), then inject sidecar | Calls `GetClient(false)` to run Connect flow on itself |
| `Leave` | user daemon | **user only** | ❌ | — | Operates on `currentConnectionID` (user daemon state) |
| `Sync` | user daemon | **user only** | ❌ | user→user self-call (`Connect`) | Like Proxy: connects first, then sets up syncthing |
| `Unsync` | user daemon | **user only** | ❌ | — | Cleans up sync on `currentConnectionID` |
| `Reset` | user daemon | **user only** | ❌ | — | Resets workload spec via K8s API |
| `Uninstall` | user daemon | **user only** | ❌ | — | Deletes traffic manager deployment |
| `Status` | user daemon | **both** | ✅ line 27 | user queries sudo for TUN IPs | User: enriches with TUN IPs from sudo. Sudo: returns raw connection info |
| `Route` | user daemon | **both** | ✅ line 17 | user→sudo (resolves TUN device, forwards) | User: finds TUN IP + device name. Sudo: executes `tun.AddRoutes` |
| `Quit` | user daemon | **both** | ✅ line 42 | — | User: cleans up connections. Sudo: cleans up DNS/hosts |
| `ConnectionList` | user daemon | **both** | ✅ line 27 | user queries sudo for TUN IPs | Same pattern as Status |
| `ConnectionUse` | user daemon | **user only** | ❌ | — | Sets `currentConnectionID` |
| `Logs` | **sudo daemon** | **sudo only** | ❌ | — | Reads both daemon log files |
| `SshStart` | **sudo daemon** | **sudo only** | ❌ | — | Creates TUN device (needs root) |
| `SshStop` | **sudo daemon** | **sudo only** | ❌ | — | Stops SSH TUN server |
| `Identify` | user daemon | **both** | ❌ | — | Returns `svr.ID` (works in either) |
| `Upgrade` | user daemon | **both** | ❌ | — | Version comparison (works in either) |
| `Version` | user daemon | **both** | ❌ | — | Returns `config.Version` (works in either) |

### Internal helpers (not RPC)

| Method | Runs in | Purpose |
|---|---|---|
| `redirectConnectToSudoDaemon` | user only | Control plane: create traffic mgr, generate OwnerID/ConnectionID, forward to sudo |
| `forwardConnectToSudo` | user only | Create outbound pod, upgrade deploy, forward connect request to sudo daemon |
| `detectAndSetManagerNamespace` | user only | Auto-detect which namespace has the traffic manager |
| `findConnection` | both | O(n) lookup by ConnectionID |
| `removeConnection` | both | Remove all matching connections |
| `resetCurrentConnection` | both | After removal, pick first remaining as current |
| `initStreamLogger` | both | Create per-RPC logger with StreamHook |
| `getSudoTunIPs` | user only | Query sudo daemon Status for TUN IPs |
| `LoadFromConfig` | user only | Restore connections on restart |
| `OffloadToConfig` | user only | Persist connections to disk |
| `CleanupConfig` | user only | Delete persisted config |

## Data flow: who calls whom

```
CLI ──→ User Daemon (GetClient(false))
          │
          ├── Connect ──→ redirectConnectToSudoDaemon
          │                  │
          │                  └── GetClient(true) ──→ Sudo Daemon
          │                                           └── Connect (IsSudo=true)
          │                                                └── DoConnect (TUN, DHCP, DNS)
          │
          ├── Proxy ──→ GetClient(false) ──→ self Connect flow (above)
          │              then: CreateRemoteInboundPod (inject sidecar)
          │
          ├── Sync ──→ GetClient(false) ──→ self Connect flow
          │             then: DoSync (syncthing)
          │
          ├── Disconnect ──→ GetClient(true) ──→ Sudo Daemon Disconnect
          │                   then: clean up user side
          │
          ├── Route ──→ GetClient(true) ──→ Sudo Daemon Route
          │              (user resolves TUN device, sudo executes)
          │
          ├── Status ──→ getSudoTunIPs (queries sudo Status)
          │               then: enrich with TUN IP info
          │
          └── Leave/Reset/Uninstall/Unsync ──→ direct (user daemon only)

CLI ──→ Sudo Daemon (GetClient(true))
          │
          ├── Logs (reads both log files)
          ├── SshStart (creates TUN, needs root)
          └── SshStop
```

## Potential issues found

### ✅ Correct patterns

1. **Connect**: properly branches on `IsSudo` — user daemon does control plane, sudo daemon does data plane
2. **Disconnect**: correctly disconnects sudo first, then user (because SSH jump is in user daemon)
3. **Route**: user resolves TUN device name (needs `getSudoTunIPs`), forwards to sudo for `tun.AddRoutes` (needs root)
4. **Status/ConnectionList**: user enriches with TUN IPs from sudo
5. **Proxy/Sync**: self-call via `GetClient(false)` to reuse Connect flow — correct, not a bug

### ⚠️ Methods without IsSudo guard

These methods run in whichever daemon receives the RPC. If called on the wrong daemon, they'll fail silently or produce wrong results:

| Method | Expected daemon | Risk if called on wrong daemon |
|---|---|---|
| `Leave` | user | Sudo has no `currentConnectionID` → "no connection found" |
| `Unsync` | user | Same — no sync state in sudo |
| `Reset` | user | Would work (creates own K8s client) but shouldn't be called on sudo |
| `Uninstall` | user | Would work but shouldn't be called on sudo |
| `Logs` | sudo | Would work on user but only reads local files — no issue |
| `SshStart` | sudo | Needs root for TUN device — would fail on user daemon |
| `SshStop` | sudo | References `svr.sshCancelFunc` — would be nil on user |

None of these are actually called on the wrong daemon (CLI always uses `GetClient(false)` for user-only RPCs, `GetClient(true)` for sudo-only). But there's no compile-time or runtime guard preventing it.
