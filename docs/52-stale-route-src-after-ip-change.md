# Stale Route Source Address After a TUN IP Change (macOS)

## 1. Symptom

A connection that is up by every measure and carries nothing:

```
➜ kubevpn status
CURRENT   CONNECTION ID   CLUSTER   KUBECONFIG         NAMESPACE   STATUS      NETIF
*         f1b98492953d    default   ~/.kube/hongkong   default     connected   utun6

➜ kubevpn route search 10.42.0.15
DESTINATION   GATEWAY   RT_IFA       MTU    NETIF
10.42.0.15    <nil>     198.18.0.1   1415   utun6

➜ ifconfig utun6
utun6: flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST> mtu 1415
    inet 198.18.0.2 --> 198.18.0.2 netmask 0xffffffff
```

Pod IPs, service IPs, cluster DNS and other clients' TUN IPs all fail at once. Nothing in the
route table looks wrong, and the client log says nothing — the daemon is only noisy at Debug.

One column gives it away. `RT_IFA` is the route's pinned source address (`RTAX_IFA`, printed by
`cmd/kubevpn/cmds/route.go` from what `netroute` reports), and it names an address the device
does not have. `198.18.0.2` is ours; `198.18.0.1` belongs to a different client of the same
traffic manager.

## 2. Root Cause

macOS resolves the source address for a route **once, when the route is created**, and stores it
in the entry. Nothing re-derives it afterwards. So when the TUN address changes —
`NetworkManager.ChangeTunIP` → `tun.ChangeIP`, driven by the lease push in
[09-tun-ip-hot-update.md](09-tun-ip-hot-update.md) — every route installed under the old address
keeps sourcing packets from it, even though it has been removed from the interface.

Outbound packets then leave carrying an address that belongs to somebody else. The server's
`RouteHub` either delivers the reply to that other client's connection or drops it outright
(`No route for stack output`, see [51-idle-client-route-registration.md](51-idle-client-route-registration.md)),
so the local machine sees total silence. Heartbeats keep the connection "healthy" because the
control conn is a separate path, which is why `status` stays green.

This is the second failure mode of the same change.
[45-tun-ip-change-stale-address.md](45-tun-ip-change-stale-address.md) fixed the first one — the
old address lingering on the device as an alias. With the address now genuinely removed, the
routes that still point at it became the next way to arrive at the same silence.

### 2.1 Why nothing recovered

Three independent gaps had to line up, and all three were present:

| Gap | Where | Effect |
|---|---|---|
| The refresh covered only the cluster CIDRs computed at connect | `refreshCIDRRoutes` | `/32` host routes (`AddRoute`) and pod prefixes pushed by the route watcher (`AddRouteCIDR`) kept the stale source |
| "Already routed through the TUN" was judged by interface name alone | `AddRoute`, `AddRouteCIDR` | A route with a dead source satisfies that test, so every later attempt skipped it — the state could not be re-entered |
| `RTM_ADD` returning `EEXIST` was treated as success | `pkg/tun/route_darwin.go` | A re-add silently did nothing, so even an in-scope refresh could be a no-op with no log line |

Individually each is reasonable. Together they make the damage permanent: the one event that
could repair the table (installing the route again) was both out of scope and short-circuited,
and the short circuit left no trace.

## 3. Fix

### 3.1 Re-assert, do not just add (`pkg/tun/route_darwin.go`)

`addRoute` no longer swallows `EEXIST`. It retries the same prefix as `RTM_CHANGE`, which makes
the kernel re-derive the ifa from the gateway. Re-asserting is now the operation that corrects a
stale source, and it corrects it **in place** — unlike a delete/add pair, the route is never
briefly absent.

### 3.2 Refresh what the kernel has, not what we remember (`pkg/handler/network.go`)

`refreshCIDRRoutes` became `refreshTunRoutes`, driven by `tun.ListRoutes` — the routes the kernel
currently holds on the device, identified by `RTF_STATIC` (the flag kubevpn always sets; the
kernel's own scope entries for the device address do not carry it and are left alone).

The kernel is the source of truth here on purpose. Routes reach a TUN device from four places —
device creation in `tun_darwin.go`, `AddRoute`, `AddRouteCIDR`, and the `kubevpn route add` RPC in
`daemon/action/route.go` — and a remembered list stops covering them the moment a fifth appears,
which is exactly how the first gap came about. Linux and Windows choose the source address at
transmit time, so `ListRoutes` reports nothing there and the refresh is a no-op.

### 3.3 Judge a route by its source, not just its device (`pkg/handler/network.go`)

`routedViaTunWithCurrentSrc` replaces the `ifi.Name == nm.tunName` test in `AddRoute` and
`AddRouteCIDR`. `netroute` already returned the source address; it was being discarded. A route
whose source is not the TUN's current address is no longer "already installed", so the ordinary
route-watcher push repairs it.

### 3.4 Notice it without being asked (`pkg/handler/network.go`)

`repairStaleRouteSrc` runs in `watchDataPlaneLiveness` whenever the watchdog tears a session
down. Before blaming the port-forward it asks the kernel which source a cluster destination would
get; a mismatch is reported by name — dead address and current address both — through
`plog.Throttle`, and the routes are re-asserted.

This matters more than the specific gaps it covers. The watchdog's own diagnosis (reconnect the
port-forward) is useless against a stale source, so without this check the daemon loops on the
wrong remedy indefinitely — 20 hours of it in the incident behind
[51-idle-client-route-registration.md](51-idle-client-route-registration.md). A fifth route source
appearing later lands here too, visibly, instead of going dark.

## 4. Tests

- `pkg/tun/ip_change_root_darwin_test.go: TestTUN_ChangeIP_RouteSrcFollowsNewAddress`
  (`-tags tun`, root, macOS) — real utun, a cluster prefix and a `/32`, a real `ChangeIP`, then
  `RTAX_IFA` read back from the kernel. The only test that exercises the `RTM_CHANGE` fallback.
- `pkg/handler/route_src_integration_test.go: TestIntegration_RouteSrc_SurvivesTunIPChange` —
  renting an IP over the real gRPC `TunConfigService`, then an eight-phase story (install from all
  three sources → healthy re-announcement must not churn → IP change → refresh → regression to the
  stale state → watcher push repairs it → watchdog repairs it unprompted → healthy table stays
  quiet). The routing table is faked, but faked around the one rule that causes the bug: an entry
  remembers the address present when it was installed. That makes the whole path reproducible on
  any platform, without root and without a device.

## 5. Field Checklist

When a connection is "connected" and carries nothing on macOS:

```sh
kubevpn route search <any-pod-ip>   # note RT_IFA
ifconfig <netif>                    # note inet
```

If `RT_IFA` is not the device's `inet`, this is the bug. `netstat -rn -f inet | grep <netif>`
shows how much of the table is affected. In the daemon log, `TUN IP changed` marks the trigger and
`Route to ... still sources from ...` marks the repair.
