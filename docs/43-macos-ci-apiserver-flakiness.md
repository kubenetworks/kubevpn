# macOS CI apiserver Flakiness vs. Mesh e2e Failures

## 1. Symptom

On CI, the `pkg/handler` e2e suites (`TestFunctions`, `TestSSHFunctions`) fail on **macOS**
while the **linux** job runs the identical `make ut` suite green. The failing subtests are
always the **mesh** (header-routing) ones:

- `serviceMeshReviewsServiceIP` (its sibling `serviceMeshReviewsPodIP` may pass)
- `serviceMeshReviewsServiceIPPortMap`
- `kubevpnRunWithServiceMesh` (incl. `kubevpnRunWithServiceMeshStatus`)

Full-proxy variants (`centerProxyServiceReviewsServiceIP`, `kubevpnRunWithFullProxy`,
`kubevpnSyncWithFullProxy`) pass on the same run.

## 2. Root cause — infra, not mesh code

The macOS GitHub runner runs minikube inside a nested VM (GitHub macOS VM → lima/qemu →
minikube). Under the sustained load of the ~90-minute handler e2e run, the Kubernetes
**apiserver stalls for minutes at a time** (`net/http: TLS handshake timeout`). During each
stall the `kubevpn-traffic-manager` pod's endpoint blackouts (or the pod is evicted entirely),
so the injected envoy sidecar's ADS/xDS stream to `xds_cluster`
(`kubevpn-traffic-manager.<ns>:9002`, `STRICT_DNS`) resets and then stays down with
`no healthy upstream` until the apiserver recovers.

Mesh assertions require a **live ADS stream** — matched requests are routed by envoy back to
the developer's machine, and even unmatched (origin) requests ride envoy's per-port loopback
cluster. When the ADS stream is dead, those requests 503 / time out. Full-proxy assertions
survive because they do not depend on live header-based routing to the same degree.

The mesh code itself is correct — proven by the linux job passing the same tests. Relevant
mesh fixes already in place: loopback origin return path (`dc734d28`,
[42-origin-loopback-cluster.md](42-origin-loopback-cluster.md)), ADS keepalive on the
`xds_cluster` bootstrap, namespace-migration re-injection (`injectedForManager` in
`pkg/inject/mesh.go`), and fatal-with-retry `TunConfigServer` init (`dcc23c52`).

### Evidence (run `/data/logs_76992757357`, PR #784)

| Job | `pkg/handler` result | duration | apiserver |
|---|---|---|---|
| **linux** | `ok … 1040s` (all mesh subtests pass) | ~17 min | stable |
| **macos** | `FAIL … 5830s` | ~97 min | two `TLS handshake timeout` storms |

- macOS apiserver stalls: `09:54–09:59` (apiserver `127.0.0.1:49719`) and `10:36–10:38+`
  (`127.0.0.1:32771`) — both suites hit it independently.
- envoy sidecar ADS timeline: OK at `09:50`, first reset `09:56`
  (`upstream_reset_after_response_started{connection_termination}`), permanent from `09:58`
  (`no healthy upstream` for 600+ s).
- Correlated: `09:59` `no running pod with label app=kubevpn-traffic-manager: not found` —
  the traffic-manager pod was gone during the stall.
- The mesh `healthChecker` retry budget was `30 × 5s = 150s`, **shorter** than the 3–5 min
  stall, so the assertion exhausted mid-outage and failed.
- The port-map assertion getting `local pc` instead of `local pc 8080` is a **symptom** of the
  dead ADS stream (routing falls back to `loopback:9080`), not an independent bug.

## 3. Triage rule

> Mesh subtests fail on macOS **but the linux job passes the same tests**, and the log shows
> `net/http: TLS handshake timeout` / `no running pod with label app=kubevpn-traffic-manager`
> around the failure window ⇒ **infra flakiness, not a mesh code regression.**

## 4. Mitigations

**A. Load reduction (`.github/workflows/test.yml`, `Makefile`).** The two heavy suites already
run serially in one job (`go test -p=1`, no `t.Parallel()`), so a single cluster carries the
full ~90 min load. The macOS job is now a `strategy.matrix` over
`suite: [ '^TestFunctions$', '^TestSSHFunctions$' ]`, each leg on its own minikube. Selection
is via the `E2E_RUN` `-run` filter on the `ut` Makefile target (default `.`, so linux/coverage
are unchanged). Halving each cluster's sustained load shrinks the apiserver-degradation window.

**B. Test robustness (`pkg/handler`).** Mesh checks use an extended retry budget
`meshHealthCheckBackoff` (~60 × 5s ≈ 300s+, jittered) to ride out a stall instead of failing on
it, and each mesh subtest first calls a best-effort `waitManagerReady` gate (wait ≤2m for a
Ready `kubevpn-traffic-manager` pod in any namespace) so assertions do not begin against a
blacked-out control plane. The gate only logs on timeout — the extended budget is the backstop,
so a stall in the gate itself never becomes a false failure.

A and B are layered defenses, not alternatives: A lowers the chance of a stall; B tolerates one
if it still happens.

## 5. Limitations

- minikube `cpus/memory` are already `max` on the runner — no further headroom there.
- The split doubles macOS runner usage (accepted trade for reliability).
- Full mesh routing is only observable in-cluster (CI minikube/lima); local arm64 TUN cannot
  reach a remote cluster, so these paths cannot be reproduced on a dev macOS box.

See also [40-envoy-original-dst-loop.md](40-envoy-original-dst-loop.md) and
[42-origin-loopback-cluster.md](42-origin-loopback-cluster.md).
