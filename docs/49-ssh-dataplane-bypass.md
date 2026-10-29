# SSH Data-plane Bypass (Auto-detect)

## Problem

The standard data-plane path tunnels all VPN traffic through `kubectl port-forward`
(SPDY/WebSocket via the Kubernetes API server). This has a well-documented instability
problem: the port-forward session can "black-hole" — the local TCP connects and looks
alive, but no data traverses (see [47-portforward-blackhole-liveness.md](47-portforward-blackhole-liveness.md)).

When the user already connects through an SSH jump host (`--ssh-addr`), and that host
happens to be a Kubernetes node, kube-proxy rules on the node make the traffic manager's
ClusterIP directly reachable. We can tunnel the data plane through SSH instead, bypassing
the API server entirely.

## Current transport lifecycle

`NetworkManager` now owns a single supervisor in `pkg/handler/network_transport.go`.
The older `probeSSHDataPlane`/`sshForward`/`portForward` flow described below is
superseded by generation-scoped sessions:

- With SSH configured, bounded `DialSshRemote` and direct-tcpip channel opens check
  all three destinations before creating `StartPortMap` handles. A local listener
  alone is not evidence of SSH reachability.
- Otherwise `StartPortForwardSession` creates all three ephemeral loopback
  endpoints using one Kubernetes upgrade. The original REST configuration,
  including any SSH-backed API-server route, is preserved.
- A generation owns its forwarding handles, pending dials, and returned data/xDS
  connections. Retirement unpublishes it and resets heartbeat generation before
  closing connections and joining handles. Late dial results cannot be published.
- On network failure the supervisor tries the other configured path, then uses
  bounded backoff. Authentication, trust, configuration, HTTP 401/403, and SSH
  administratively-prohibited failures terminate instead of falling back.
- A healthy current path is sticky. Both paths use the same data-plane heartbeat
  policy; runtime xDS watcher errors alone do not retire the data path.
- Bootstrap waits for a correlated current-generation heartbeat after TUN startup,
  within the 60-second total startup budget; listener readiness alone is insufficient.
- The integrated vendored netmon monitor only wakes the supervisor to compare current
  underlay routes/interfaces, with sticky time-jump detection. Relevant changes clear
  pending probes without changing generation, then request fresh evidence with a bounded
  recovery deadline. See [47](47-portforward-blackhole-liveness.md) for route sampling.
- Replacement retains the TUN and client gVisor stacks. IP watcher reconnects still
  reassert the DHCP lease and can legitimately update the address. Shutdown cancels the
  parent first, retires the active generation, then waits startup/workers before clearing state.
- No CLI flags, proto fields or dependency changes were added for this lifecycle.

## Original design (historical)

The following auto-detection flow, comparison, liveness notes and code-location table
preserve the original implementation. `probeSSHDataPlane`/`sshForward`/`portForwardOnce`
and `PortMapUntil` recovery are superseded by the current supervisor above; SSH is not
assumed immune to black holes.

### Auto-detection (zero new flags)

When `--ssh-addr` is provided, the root daemon automatically probes whether the SSH host
can reach the traffic manager's ClusterIP:

```
NetworkManager.Start()
  │
  ├─ probeSSHDataPlane()
  │    1. Get Service "kubevpn-traffic-manager" → ClusterIP
  │    2. SSH dial → tcp://<ClusterIP>:10801 (3s timeout)
  │    3. Success → return (clusterIP, true)
  │       Failure → return ("", false)
  │
  ├─ [probe OK]  → sshForward()   — SSH tunnel for all 3 ports
  └─ [probe fail] → portForward() — standard kubectl port-forward (unchanged)
```

No new CLI flags, no new proto fields. Bastion-only SSH hosts (not K8s nodes) fail the
probe silently and fall back to port-forward — fully backward compatible.

### Port forwarding via SSH

`sshForward()` calls `ssh.PortMapUntil()` (existing helper in `pkg/ssh/tunnel.go`) for each
port pair. This establishes local TCP listeners that tunnel through the SSH connection to
the traffic manager's ClusterIP:

```
127.0.0.1:<local1>  ──SSH──▶  <ClusterIP>:10801  (gvisor TCP data plane)
127.0.0.1:<local2>  ──SSH──▶  <ClusterIP>:10802  (gvisor UDP-over-TCP data plane)
127.0.0.1:<local3>  ──SSH──▶  <ClusterIP>:9002   (xDS gRPC control plane)
```

After the tunnels are established, the rest of the connect flow is unchanged: TUN dials
`tcp://127.0.0.1:<local1>`, gRPC dials `127.0.0.1:<local3>`, heartbeat liveness works
at the TUN layer regardless of the underlying transport.

### Service port 10802

Port 10802 (UDP-over-TCP) was previously only accessible via direct pod port-forward.
It is now exposed on the `kubevpn-traffic-manager` Service as TCP so it is reachable via
ClusterIP. This is required for SSH-mode and harmless for the standard port-forward path
(which already forwarded to the pod directly).

## Architecture comparison

```
Standard path (port-forward):
  TUN ─▶ 127.0.0.1:<local> ─▶ [SPDY/WS via API server] ─▶ pod:10801

SSH data-plane path:
  TUN ─▶ 127.0.0.1:<local> ─▶ [SSH tunnel] ─▶ node ─▶ ClusterIP:10801 ─▶ pod:10801
```

| Aspect | port-forward | SSH data-plane |
|---|---|---|
| API server dependency | Yes (SPDY/WebSocket) | No |
| Black-hole risk | Known issue | SSH TCP is reliable |
| Reconnection | Client-side detect + retry | `PortMapUntil` auto-reconnects |
| Pod restart handling | Must re-find pod | ClusterIP is stable; kube-proxy updates endpoints |
| Requirement | kubeconfig access | SSH access to a K8s node |

## Liveness

- **Heartbeat watchdog** (`watchDataPlaneLiveness`): unchanged — operates at TUN layer
- **Pod status watcher** (`CheckPodStatus`): not needed in SSH mode (ClusterIP is stable),
  but the code path is simply not reached (SSH mode does not call `portForwardOnce`)
- **SSH reconnection**: `PortMapUntil` internally reconnects SSH clients via its
  `connPool` (single-flight creation, health-aware eviction — see `pkg/ssh/pool.go`)

## Code locations

| Component | File | Function |
|---|---|---|
| Probe | `pkg/handler/network.go` | `probeSSHDataPlane()` |
| SSH tunnel setup | `pkg/handler/network.go` | `sshForward()` |
| Branch in Start() | `pkg/handler/network.go` | `Start()` |
| SSH config threading | `pkg/handler/data_session.go` | `DataSession.SshConf` |
| Root daemon wiring | `pkg/daemon/action/connect.go` | `Connect()` |
| Service port 10802 | `pkg/handler/traffmgr_resources.go` | `genService()` |
| Port name constant | `pkg/config/config.go` | `PortNameUDP` |
| SSH port forwarding | `pkg/ssh/tunnel.go` | `PortMapUntil()` |

## Related docs

- [15-ssh-architecture.md](15-ssh-architecture.md) — SSH subsystem overview
- [47-portforward-blackhole-liveness.md](47-portforward-blackhole-liveness.md) — port-forward instability
- [24-traffic-manager-deployment.md](24-traffic-manager-deployment.md) — traffic manager resources
- [02-dual-daemon.md](02-dual-daemon.md) — user daemon vs root daemon
