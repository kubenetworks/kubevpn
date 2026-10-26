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

## 3. The Same Trigger on Other Platforms

The pinned-source mechanism is specific to macOS. The **trigger** — replacing the TUN address on
a live device — is not, and on Linux it produces an outage that looks identical from the outside
and is in one respect worse.

| | How routes are installed | When the source address is chosen |
|---|---|---|
| macOS | `route_darwin.go` writes `RTM_ADD`; the kernel stamps `RTAX_IFA` into the entry | At creation, never recomputed — the bug above |
| Linux | `route_linux.go` calls `netlink.AddRoute` with no source, so the entry has no prefsrc | At transmit, by `inet_select_addr` |
| Windows | `route_windows.go` calls `AddRoute` on the interface LUID; `MIB_IPFORWARD_ROW2` has no source field at all | At transmit, by the stack |

### 3.1 Linux: the routes do not go stale, they disappear

`changeIP` deleted the old address before adding the new one. Linux flushes a device's entire
IPv4 route table the instant it loses its **last** IPv4 address, so that ordering did not leave a
stale route — it left no route. Nothing puts them back: the refresh is gated on darwin, and the
cluster CIDR installed at connect is never re-pushed by anything.

Worse than the outage: while the device is unaddressed, destinations that belonged to the tunnel
match the default route instead, so cluster traffic **leaves over the physical NIC** rather than
failing closed.

Measured on a real TUN device (`TestTUN_ChangeIP_RoutesSurviveAndFollowNewAddress`, before the
fix):

```
route 10.151.0.0/16 vanished when the address changed  (remaining: fe80::/64)
route 10.152.0.10/32 vanished when the address changed
Route(10.151.0.1) resolved to eth0, want utun0
```

The fix is ordering, not restoration: add the new address first, then delete the old one
(`pkg/tun/ip_linux.go`). The device is addressed throughout, the routes are never flushed, and
there is no window in which traffic can leak. Restoring the routes afterwards would have left
that window open.

macOS keeps the opposite order deliberately — see
[45-tun-ip-change-stale-address.md](45-tun-ip-change-stale-address.md), where deleting the old
alias first is what the fix was — and does not need this one: its routes survive, which is how
they come to be stale in the first place.

### 3.2 IPv6

IPv6 does not share the failure, on either platform — but for a reason that is easy to get
wrong, and that this document got wrong in its first version.

**macOS pins the interface's link-local address into IPv6 routes, not the global one.** Measured
on a real device: a route to `2001:db8:144::/64` reports
`RT_IFA = fe80::a4e0:bb39:154f:25fe` while a socket to that same destination sources from
`2001:2::…`. BSD hands `rt_ifa` straight to `ip_output` for IPv4, but for IPv6 it runs
`in6_selectsrc` (RFC 6724) at transmit time against the addresses the device holds *now*. So the
address recorded in an IPv6 route is one that never goes away, and the address packets actually
carry is never stale.

**Linux agrees, by a different route:** the global address is what both `ip route get` and a
connected socket report (`2001:2::161` in the measurement), and the IPv4 flush of section 3.1
has no IPv6 counterpart — a device keeps its `fe80::/64` for its whole life, so it never loses
its *last* IPv6 address and the flush never fires. Running the dual-stack test against the old
delete-first ordering shows the asymmetry directly:

```
route 10.151.0.0/16 vanished when the address changed
  (remaining: 2001:2::152/128, 2001:db8:151::/64, fe80::/64)
packets to 10.151.0.1 would leave via eth0, not utun0
```

This cost a round trip. `srcIsCurrentTunIP` first demanded that an IPv6 route's recorded source
equal the TUN's global address — which on macOS is **permanently false**. Every route-watcher
push would have reinstalled every IPv6 route, and the watchdog would have reported "outbound
packets carry a dead address" at a perfectly healthy tunnel, on every teardown, forever: the
precise kind of confident wrong diagnosis
[51-idle-client-route-registration.md](51-idle-client-route-registration.md) is about. The check
now asks for equality on IPv4 (where the recorded address *is* the source) and mere membership
in the device's current addresses on IPv6 (which still catches a source that has been removed).

What that round trip also established, on a real macOS kernel: IPv6 routes install fine despite
`RTF_CLONING`, and `listTunRoutes` enumerates them — both previously unverified.

**Methodology.** Judge a source address by dialling, not by reading the route table.
`connect(2)` on a UDP socket makes the kernel perform the full route lookup and source
selection and sends no packet, so `LocalAddr` is the kernel's own answer to the only question
that matters. The shared helper is `kernelSourceFor` in `pkg/tun/src_probe_tun_test.go`, used by
both root tests. Reading `RTAX_IFA` answers a different question on macOS IPv6, and that is how
the wrong conclusion got into this file.

### 3.3 Windows: unverified

Windows is reasoned about, not measured. Its routes are bound to the interface LUID and
`MIB_IPFORWARD_ROW2` carries no source address, so neither failure mode should apply; `changeIP`
there still deletes before adding. No Windows machine was available, and the Windows CI job
(`make ut-no-cluster`) cannot create a TUN device, so this is stated as an open item rather than
a conclusion. If a Windows user ever reports the symptom in section 1, check first whether the
device's routes survive an IP change.

## 4. Fix

### 4.1 Re-assert, do not just add (`pkg/tun/route_darwin.go`)

`addRoute` no longer swallows `EEXIST`. It retries the same prefix as `RTM_CHANGE`, which makes
the kernel re-derive the ifa from the gateway. Re-asserting is now the operation that corrects a
stale source, and it corrects it **in place** — unlike a delete/add pair, the route is never
briefly absent.

### 4.2 Refresh what the kernel has, not what we remember (`pkg/handler/network.go`)

`refreshCIDRRoutes` became `refreshTunRoutes`, driven by `tun.ListRoutes` — the routes the kernel
currently holds on the device, identified by `RTF_STATIC` (the flag kubevpn always sets; the
kernel's own scope entries for the device address do not carry it and are left alone).

The kernel is the source of truth here on purpose. Routes reach a TUN device from four places —
device creation in `tun_darwin.go`, `AddRoute`, `AddRouteCIDR`, and the `kubevpn route add` RPC in
`daemon/action/route.go` — and a remembered list stops covering them the moment a fifth appears,
which is exactly how the first gap came about. Linux and Windows choose the source address at
transmit time, so `ListRoutes` reports nothing there and the refresh is a no-op — Linux needs the
ordering fix in section 3.1 instead.

### 4.3 Judge a route by its source, not just its device (`pkg/handler/network.go`)

`routedViaTunWithCurrentSrc` replaces the `ifi.Name == nm.tunName` test in `AddRoute` and
`AddRouteCIDR`. `netroute` already returned the source address; it was being discarded. A route
whose source is not the TUN's current address is no longer "already installed", so the ordinary
route-watcher push repairs it.

### 4.4 Notice it without being asked (`pkg/handler/network.go`)

`repairStaleRouteSrc` runs in `watchDataPlaneLiveness` whenever the watchdog tears a session
down. Before blaming the port-forward it asks the kernel which source a cluster destination would
get; a mismatch is reported by name — dead address and current address both — through
`plog.Throttle`, and the routes are re-asserted.

This matters more than the specific gaps it covers. The watchdog's own diagnosis (reconnect the
port-forward) is useless against a stale source, so without this check the daemon loops on the
wrong remedy indefinitely — 20 hours of it in the incident behind
[51-idle-client-route-registration.md](51-idle-client-route-registration.md). A fifth route source
appearing later lands here too, visibly, instead of going dark.

## 5. Tests

- `pkg/tun/ip_change_root_darwin_test.go: TestTUN_ChangeIP_RouteSrcFollowsNewAddress`
  (`-tags tun`, root, macOS) — real utun, a cluster prefix and a `/32`, a real `ChangeIP`, then
  `RTAX_IFA` read back from the kernel. The only test that exercises the `RTM_CHANGE` fallback.
- `pkg/tun/ip_change_root_linux_test.go: TestTUN_ChangeIP_RoutesSurviveAndFollowNewAddress`
  (`-tags tun`, root, Linux) — the test that found section 3.1. Dual stack, and it asserts both
  halves of the claim that Linux is unaffected: the routes still exist after an address change,
  and the source the kernel would now pick is the new address. It asks the kernel directly
  (`netlink.RouteGet`, i.e. `ip route get`) rather than going through netroute, which substitutes
  the interface's first address when a route carries no prefsrc — for IPv6 usually the
  link-local one, which would answer a different question. It runs anywhere with `/dev/net/tun`.
- `pkg/handler/route_src_integration_test.go: TestIntegration_RouteSrc_SurvivesTunIPChange` —
  renting an IP over the real gRPC `TunConfigService`, then an eight-phase story (install from all
  three sources, both families → healthy re-announcement must not churn → IP change → refresh →
  regression to the stale state → watcher push repairs it → watchdog repairs it unprompted →
  IPv6 alone goes stale and must still be caught → healthy table stays quiet). The routing table is faked, but faked around the one rule that causes the bug: an entry
  remembers the address present when it was installed. That makes the whole path reproducible on
  any platform, without root and without a device.

## 6. Field Checklist

When a connection is "connected" and carries nothing on macOS:

```sh
kubevpn route search <any-pod-ip>   # note RT_IFA
ifconfig <netif>                    # note inet
```

If `RT_IFA` is not the device's `inet`, this is the bug. `netstat -rn -f inet | grep <netif>`
shows how much of the table is affected. In the daemon log, `TUN IP changed` marks the trigger and
`Route to ... still sources from ...` marks the repair.
