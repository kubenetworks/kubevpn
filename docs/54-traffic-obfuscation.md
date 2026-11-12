# 54 — Data-Plane Stream Obfuscation & TLS Enforcement

## Problem

Two weaknesses made the kubevpn data plane identifiable and, in one path, unencrypted:

1. **Static wire signature.** The gtcp stream (10801) carries TLS records whose plaintext
   frames follow the fixed `[2-byte BE length][1-byte type][IP packet]` layout of
   `pkg/internal/wire`. A passive middlebox that terminates/forges TLS, or any classifier
   with TLS visibility (corporate MITM proxies), can match the record-size distribution and
   inner frame structure to kubevpn and selectively throttle or block it. This is the same
   problem WireGuard has (fixed 148-byte handshake/messages); Tailscale has **no**
   obfuscation today (tailscale/tailscale#13119 remains open), so the borrowed designs are
   Mullvad 2025 (QUIC/MASQUE mimicry + LWO lightweight obfuscation: header scrambling +
   random padding) and AmneziaWG (Jc junk packets + Jmin/Jmax padding).
2. **Raw-TCP fallback.** `ErrNoTLSConfig` degraded both the client Transporter and the
   server Listener to plaintext TCP with only a warning.

## Stage 1 — TLS is now mandatory (no plaintext fallback)

- `TCPTransporter` (`pkg/endpoint/transporter_tcp.go`) returns a `failTransporter` that
  refuses to dial when TLS material is absent or invalid.
- `GvisorTCPListener` (`pkg/endpoint/listener_tcp.go`) closes the listener and returns an
  error naming the required env (`tls_crt` / `tls_key` / `tls_server_name`).

**Zero production impact**: the traffic-manager unconditionally creates the TLS Secret
(`pkg/cluster/manager/manager.go`), the server container injects it via `EnvFrom`
SecretRef, and sidecars get the env explicitly (`pkg/inject/container.go`). The only
affected path is running `kubevpn server` locally **without** the TLS env, which now fails
at startup with an actionable message instead of silently running plaintext. That is the
intended behavior change.

## Stage 2 — stream obfuscation (on by default, opt-out)

### Design: stream-level `net.Conn` wrapper, not frame-level

Investigation showed frame I/O is produced from six paths (`wire.Conn`, `BatchWriter`,
route writes in `route.go`, `bufferedTCP`, control-conn writes, generation writes), so
frame-level transforms would need a producer registry and still miss paths. Instead the
transform wraps the whole TCP stream at exactly the two connection-establishment points:

```
client: UDPOverTCPConnector.ConnectContext   → ObfuscateDial(conn) → framing.NewConn
server: gvisorTCPHandler.Handle              → ObfuscateAccept(conn) → newBufferedTCP
```

Everything above the wrapper (framing, batching, routing, heartbeats, slot logic) is
unaware of it; everything below (TLS) sees uniformly random bytes.

### Protocol

- The dialer generates a 16-byte `crypto/rand` seed and prepends it to its **first
  write**; the acceptor consumes it on its **first read**. Lazy on both sides: the
  constructors do no I/O and add zero latency at other establishment paths.
- Both directions derive independent keystreams from the seed:
  `base = seed[0:8] ^ seed[8:16]`, then direction-mixed
  (`base ^ dir*0x9E3779B97F4A7C15`) into an **xorshift64\*** stream. Word-wise
  generation keeps the transform at ~0.3 ns/byte.
- Because TCP delivers each direction in order and exactly once, the keystreams stay in
  lockstep; a mid-stream error forces connection close (the existing framing contract),
  so desynchronization is never observable on a live connection.
- The acceptor writes only after it has read the seed — guaranteed by the gtcp protocol
  where the client always speaks first; violation returns
  `errObfuscationWriteBeforeSeed`.

### Enabling

**On by default**; `KUBEVPN_OBFUSCATE=0` (or `false`/`off`) opts out. Both endpoints read
the same source, so they cannot disagree:

| Path | Source |
|---|---|
| Root-daemon client (in-cluster) | traffic-manager **Secret** key `KUBEVPN_OBFUSCATE` — read by `TCPTransporter(tlsSecret)` |
| Server / sidecar | environment variable (Secret `EnvFrom` / `inject.tlsEnvVars` propagation) |
| Local client | environment variable |

`netutil.ObfuscationWanted(map[string][]byte)` implements the lookup: secret key first,
then env; `0`/`false`/`off` (case-insensitive, trimmed) disables, anything else —
including absence — stays on.

**Mixed-version rollout warning (important)**: an OLD client (no obfuscation code)
connecting to a NEW manager sends plaintext frames that the new manager XOR-decodes —
the connection garbles and the watchdog reconnect-loops. During a rollout window where
old clients may still connect, set `KUBEVPN_OBFUSCATE=0` in the traffic-manager Secret
and remove it once the fleet is upgraded (the manager is created/upgraded by the
connecting client, so normal kubevpn operation converges versions quickly). See also:
[33-client-upgrade.md](33-client-upgrade.md) — for detailed rollout guidance.

**Caveat — TLS secret rotation**: `pkg/cluster/manager/upgrade.go` regenerates the
traffic-manager Secret on upgrade (`rotateTLSSecret`). A manually added
`KUBEVPN_OBFUSCATE` entry in that Secret is lost on rotation and must be re-applied —
note that with the on-by-default semantics, losing a "0" (opt-out) entry silently
re-enables obfuscation.

### What it is not

- **Not confidentiality**: the seed is transmitted in the clear and the keystream is a
  plain xorshift — anyone who can read the stream can strip the obfuscation. TLS remains
  the confidentiality layer; the obfuscation only removes the static
  `[length][type][packet]` signature from the TLS record plaintext.
- **No length or timing cover**: frames keep their real sizes — the XOR transform does
  not pad. Record-size and timing fingerprints remain (a stage-A uTLS/MASQUE follow-up
  would address those).
- The `gudp` (10802) relay and the xDS/gRPC control plane are untouched — they carry no
  wire framing and no wrap points.

## Performance

Measured on this branch (loopback, `-benchtime=3s`, medians of 3–5 runs):

| Benchmark | off (baseline = HEAD) | on | delta |
|---|---|---|---|
| `BenchmarkFramedBatchWrites` | ~1080–1830 MB/s | ~1240 MB/s | pure-write path cost |
| `CrossClientThroughput crossclient/flows=1` | ~79 MB/s | ~72 MB/s | ~7–9% |
| `CrossClientThroughput cluster/flows=1` | ~125 MB/s | ~116 MB/s | ~6–8% |
| `CrossClientThroughput cluster/flows=8` | ~85 MB/s | ~85 MB/s | within noise |

- **Off-state**: identical to pre-change baseline (the only added work is one
  `atomic.Bool` load per connection establishment) — but with obfuscation on by
  default, the off state is now the explicit opt-out.
- **On-state (the default)**: 6–10% on the CPU-bound single-flow loopback paths. This
  is the intrinsic cost of a software stream cipher (~0.3 ns/byte at each endpoint:
  read-side in-place XOR, write-side fused copy+XOR — the caller's buffer is never
  mutated) on a benchmark whose bottleneck is user-space CPU. Over a real WAN, where
  throughput is bounded far below loopback saturation, the CPU headroom is idle and the
  regression is not measurable. Operators who need the last few percent can set
  `KUBEVPN_OBFUSCATE=0`.

The original plan's ≤5% on-state target is met on multi-flow paths but exceeded (~6–9%)
on the single-flow CPU-saturated ones; two optimization rounds (64KB-block keystream →
word-wise xorshift → fused copy+XOR) landed at that floor, and the residual cost is the
transform itself, not its implementation.

## Files

- `pkg/internal/wire/obfuscate.go` — the `obfuscatedConn` wrapper, keystream, lazy seed
- `pkg/util/netutil/tls.go` — `ObfuscationWanted`
- `pkg/config/config.go` — `ObfuscationKey` constant
- `pkg/endpoint/transporter_tcp.go`, `listener_tcp.go` — flag set at establishment
- `pkg/endpoint/connector_udp_over_tcp.go` — client wrap point
- `pkg/tunnel/server.go` — server wrap point
- `pkg/inject/container.go` — sidecar env propagation

Tests: `wire/obfuscate_test.go` (round-trip, wire bytes differ, keystream advancement,
write-before-seed, framed stack), `endpoint/serve_integration_test.go`
(`TestServe_ObfuscatedOverTLS` — full TCP+TLS+obfuscation+framing stack),
`endpoint/protocol_test.go` (flag propagation), `inject/container_test.go` (env
propagation), `netutil/tls_test.go` (`TestObfuscationWanted`).

## Future directions (not implemented)

- **Stage A — TLS-mimicry**: uTLS client fingerprints + record-size padding to imitate
  ordinary HTTPS, addressing the size/timing fingerprints this stage leaves open.
- **TLS for gudp (10802)**: the UDP relay is currently the only unencrypted data path.
- **MASQUE/QUIC transport** (Mullvad-style): a protocol-level change, evaluated and
  deferred.
