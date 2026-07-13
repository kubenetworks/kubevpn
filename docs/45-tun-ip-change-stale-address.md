# Stale IPv4 Address After TUN IP Change Causes Outbound Failure (macOS)

## Symptom

Two clients on the same VPN: A (local machine) cannot actively `ping` / connect to peer B's
TUN IP, but:

- Reverse direction works: B `ping`/`ssh` to A works fine.
- `kubevpn route search <B>` shows routes are correct.
- TCP (e.g. SSH) can establish connections (the tunnel itself is functional).

That is, **only outbound traffic initiated by the local machine fails**, while inbound
and reverse TCP both work.

## Root Cause

macOS `SIOCAIFADDR` only **adds** an alias address — it does not replace. When the control
plane hot-updates the TUN IP (`NetworkManager.ChangeTunIP` → `tun.ChangeIP`, see
[14-rpc-daemon-mapping](14-rpc-daemon-mapping.md) and TUN_ALLOCS hot-update) from an old
value (e.g. `198.18.0.2`) to a new value (e.g. `198.18.0.5`), if the **old address is not
explicitly deleted first**, the utun interface ends up with **both old and new addresses**
present simultaneously.

macOS may then select the **old address** as the outbound source address, resulting in:

| Direction | Source / Identity | Kept alive by heartbeat | Result |
|---|---|---|---|
| Inbound (peer → new IP) | New IP (gvisor identity + heartbeat) | Yes | ✅ Works |
| Outbound (local → peer) | **Old IP (stale)** | No (heartbeat only keeps new IP alive) | ❌ The route for the old IP in the server-side RouteHub is cleared by `RemoveRoutesByConn` on read timeout → reply black hole |

The routing table appears correct because the old address is indeed still on the interface
and routes to the peer do exist. The problem is a **source-address mismatch with the
server-side registered/keepalive identity**.

### Evidence (root daemon log, 2026-07-06, current version)

```
07-06 00:00–12:48  root_daemon.log (all from the same day)
07-06 12:00  [NetworkManager] TUN IP changed: v4=198.18.0.5   # network.go:353, matches current HEAD
07-06 12:01  [Gvisor-ICMP] Echo request 198.18.0.3 -> 198.18.0.5 / Sent echo reply   # inbound .5 works
07-06 12:02–12:46  [Client] OUTBOUND SRC: 198.18.0.2 -> ...   # all outbound uses stale .2
```

- The `TUN IP changed` log line is at `network.go:353`, identical to current HEAD →
  **running the current version**, `changeIP` already includes the "delete old address first"
  call (`removeInterfaceAddress`).
- Outbound source address count: `198.18.0.5` appears **0** times, `198.18.0.2` appears
  **26** times.
- **No "Failed to remove old TUN address" warning in the entire log** → the delete call
  "returned success" but did not actually remove `.2`.

## Root Cause (Silent Delete Failure)

`changeIP` does call `removeInterfaceAddress(oldIP)` before adding the new address, but
the old IPv4 deletion uses a bare **`SIOCDIFADDR` ioctl** (`delInet4Address`), while the
address was added as a **point-to-point alias** (`setInet4Address` uses `SIOCAIFADDR`,
`dstaddr == addr`). On macOS, `SIOCDIFADDR` with only the local address and no dstaddr
**cannot match the point-to-point alias — the ioctl returns success but does not delete**,
so there is neither an error nor the address being removed. In contrast, IPv6 has always
used `ifconfig ... inet6 delete` and shows no stale addresses.

## Fix (Pure syscalls + post-delete verification, no ifconfig)

Requirement: all address deletion must use library/syscalls, not fork `ifconfig`.

1. **IPv4**: Keep `SIOCDIFADDR` ioctl (`pkg/tun/tun_darwin.go: delInet4Address`).
2. **IPv6**: Replace `ifconfig` with pure-code `SIOCDIFADDR_IN6` ioctl
   (`pkg/tun/tun_ip6del_darwin.go`). `SIOCDIFADDR_IN6 = _IOW('i', 25, struct in6_ifreq)`,
   the ioctl number embeds `sizeof(in6_ifreq)`. This struct contains a large union (largest
   member `icmp6_ifstat`), totaling 288 bytes on macOS. Recompute the number the same way as
   the *add* path (`siocAIFAddrInet6`), from the struct size:
   `(SIOCDIFADDR & 0xe000ffff) | (sizeof(in6Ifreq)<<16) == 0x81206919`.
   Since `in6_ifreq` size differs across darwin/freebsd/openbsd, split by OS: darwin uses
   ioctl, freebsd/openbsd keep ifconfig (`tun_ip6del_bsd.go`; the package is only
   compilable on darwin among BSDs anyway).
3. **Post-delete verification** (`changeIP`, pure Go): after adding the new address, read
   back via `net.InterfaceByName().Addrs()`. If the old address is still present, `Errorf`
   explicitly. A bare delete ioctl can "return success without deleting" (point-to-point
   alias matching issue); this check turns a silent degradation into a diagnosable explicit
   failure.

`GOOS=darwin` arm64/amd64 builds + vet pass. **Since this repo's CI/dev machines lack
macOS, one real-machine verification is needed for IP hot-update: `ifconfig <utunX>` should
show only the new IP, and the log should have no "still present" error.** If the check
reports the old address still present, `SIOCDIFADDR` is ineffective for point-to-point
aliases and an alternative pure-code deletion method is needed.

## Impact & Mitigation

- **Trigger condition**: TUN IP hot-update occurs while the client connection is alive
  (DHCP conflict reallocation, or manual IP change via ConfigMap `TUN_ALLOCS`). The
  first-connect direct allocation (no IP change) is unaffected.
- **Platform**: macOS only (IPv4 ioctl delete path).
- **Emergency mitigation (no upgrade needed)**: `kubevpn quit` and reconnect; or
  `sudo ifconfig <utunX> inet <oldIP> delete` to manually remove the stale address, then
  `kubevpn route search <peer>` to confirm `RT_IFA` is the new IP.

## Follow-up (Optional Hardening)

`removeInterfaceAddress` is best-effort. After deletion in `changeIP`, use
`net.InterfaceByName().Addrs()` (pure Go) to verify the old address is truly gone; if not,
retry/alert, turning "stale address" from an invisible degradation into a diagnosable
explicit failure.
