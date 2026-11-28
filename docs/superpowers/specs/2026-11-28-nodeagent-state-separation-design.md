# Node-Agent Desired/Derived State Separation Design

> Date: 2026-11-28
> Base: `master` after PR #823 (`7b7603933`)
> Status: approved for specification review

## 1. Context

PR #823 made the node-boundary DaemonSet the only traffic-attachment mode. The
remaining control-plane issue is not the data plane; it is that desired and
manager-derived Virtuals share `ENVOY_CONFIG`.

`AttachmentPublisher.reconcileOnce` currently reads that mixed list, discards
entries with `AgentOrigins`, regenerates them, and writes the combined list back
(`pkg/xds/attachment_publisher.go`). Status, leave-all, and deleted-target leave
must filter derived entries. DHCP and inject/leave also update the same key that
the publisher rewrites.

This creates avoidable coupling:

- a runtime projection is repeatedly consumed as if it were desired input;
- every consumer must know how to distinguish base and per-node entries;
- publisher writes can conflict with unrelated rule or TUN-IP updates;
- desired-state tests must also account for runtime endpoints and origins.

The data plane is explicitly out of scope for redesign. The node tunnel, direct
listeners, host DNAT/ipset behavior, OwnerID, DHCP allocation, stable listener
allocation, and declarative `SyncTarget` model remain in place.

## 2. Goals

1. Store desired base Virtuals separately from derived per-node Virtuals.
2. Give each state class one clearly defined writer set and read path.
3. Prevent the publisher from writing desired Virtuals.
4. Preserve current proxy, run, sync, Service, status, leave, reset, readiness,
   restart, and multi-user behavior.
5. Preserve the existing Virtual YAML field format, CLI parameters/output, and
   internal request semantics.
6. Detect the old mixed node-agent representation instead of silently migrating
   or continuing with ambiguous input.

## 3. Non-goals

- No dual-daemon or `Connection`/`DataPlane`/`ProxyController` redesign.
- No changes to node-agent privileges, host networking, tunnels, DNAT, Envoy
  supervision, or xDS resource generation.
- No automatic migration of mixed `ENVOY_CONFIG` written by PR #823.
- No compatibility path for legacy sidecar-injected workloads; the existing
  rejection remains.
- No `IsAgent` removal, PortMap type change, attachment RPC redesign, or status
  namespace/cache redesign in this project.
- No additional ConfigMap, JSON-Patch protocol, distributed transaction, or new
  persistence backend.

## 4. Decision: Two Keys in the Existing ConfigMap

Use the existing `kubevpn-traffic-manager` ConfigMap with four state keys:

```text
ConfigMap/kubevpn-traffic-manager
├── ENVOY_CONFIG            desired base Virtual[]
├── AGENT_XDS_CONFIG        derived per-node Virtual[] (new)
├── NODE_ATTACHMENTS        current pod/Service node attachments
└── AGENT_PORT_ALLOCATIONS  stable port assignments and tombstones
```

`ENVOY_CONFIG` and `AGENT_XDS_CONFIG` both serialize `[]*controlplane.Virtual`
with the existing YAML/JSON field names. The new key does not introduce a schema
version change: it separates records by ownership rather than changing their
fields.

A desired Virtual never contains `AgentOrigins`. A derived Virtual is generated
for a `(namespace, workload, node)` and contains the current origins and resolved
runtime endpoints. Status and lifecycle operations consume only desired state;
xDS consumes only derived state.

## 5. Ownership and Read/Write Paths

| State | Writers | Readers |
|---|---|---|
| `ENVOY_CONFIG` (desired) | inject/leave through `inject.VirtualStore`; DHCP/lease callbacks through `xds/internal/envoy.Rules`; reset through its existing update path | AttachmentPublisher, user/root status, leave-all, deleted-target leave, reset |
| `AGENT_XDS_CONFIG` (derived) | AttachmentPublisher only | xDS Watch/Processor and sync-target readiness |
| `NODE_ATTACHMENTS` | AttachmentPublisher only | node-agent runtime and direct-listener readiness |
| `AGENT_PORT_ALLOCATIONS` | AttachmentPublisher only | the next publisher reconcile |

The following rules are mandatory:

1. **The publisher never writes `ENVOY_CONFIG`.** Inject, leave, reset, DHCP, and
   lease abandonment therefore retain sole responsibility for desired changes.
2. **Status and leave do not filter derived Virtuals.** Their input key contains
   desired Virtuals by construction.
3. **xDS never falls back to `ENVOY_CONFIG`.** An absent or empty derived key is
   an empty xDS input, not a request to consume desired state.
4. **The node-agent runtime is unchanged.** It continues to consume
   `NODE_ATTACHMENTS`; it does not read either Virtual list directly.
5. **DHCP updates desired only.** The next publisher reconcile copies current
   user TUN IPs into derived rules. `SyncTarget` rules remain excluded from DHCP
   mutation and receive only the resolved clone endpoint during derivation.

## 6. Publisher Reconcile Algorithm

Each attempt performs the complete read/compute/write cycle:

1. Get the current manager ConfigMap.
2. Parse `ENVOY_CONFIG` as desired Virtuals and reject any entry containing
   `AgentOrigins` as an old mixed representation.
3. Parse the previous `NODE_ATTACHMENTS` and `AGENT_PORT_ALLOCATIONS`.
4. Build a working copy of desired Virtuals for allocation and derivation. The
   persisted desired YAML is not rewritten.
5. Resolve the newest Ready sync clone for every `SyncTarget`, list current
   source pods and Services, and construct current attachments.
6. Run the existing stable allocation helpers with the persisted allocation map.
   A desired base Virtual need not persist `agentListenerPort`: the assignment is
   restored from `AGENT_PORT_ALLOCATIONS` and written to the derived Virtual.
7. Deep-copy rules for each derived Virtual. Copy headers, PortMap, OwnerID,
   `SyncTarget`, and other rule metadata; resolve clone PodIPs only into derived
   sync rules.
8. Marshal derived Virtuals, attachments, and allocations.
9. If any of those three outputs changed, update exactly those three keys in one
   ConfigMap Update. Do not assign `ENVOY_CONFIG`.

The boolean returned by the allocation helper is not itself a write condition.
Restoring an assignment from tombstones into a fresh working copy can report a
local change while the persisted outputs are already correct. Write decisions
must compare the three persisted output values.

### Conflict handling

Wrap the complete attempt in `retry.RetryOnConflict`. On ResourceVersion
conflict, re-read the ConfigMap, re-parse the newest desired state, re-list the
current workload state, and recompute all outputs. Never retry an Update using a
stale object or the previous attempt's desired rules.

Non-conflict errors abort the attempt and are logged by the publisher run loop;
the next periodic reconcile can retry them. Existing `VirtualStore` and lease
rule writers retain their own conflict retry. Reset retains its current direct
Update behavior and returns a conflict to its caller rather than overwriting a
newer object.

## 7. Consumer Changes

### xDS

`pkg/xds/watcher.go` sends `AGENT_XDS_CONFIG` in `NotifyMessage`. DHCP/allocation
callbacks continue to run on ConfigMap changes as they do today. The initial
empty notification remains valid: a fresh manager has no derived xDS resources
until the first successful reconcile.

The processor and `pkg/xds/cache.go` continue to consume the same derived
Virtual structure. No listener, route, cluster, or endpoint generation changes
are intended.

### Status, leave, and reset

`ConnectOptions.StatusSnapshot` and `DataSession.StatusSnapshot` continue to read
`ENVOY_CONFIG`, which is now desired-only. `buildProxyAndSyncStatus` no longer
needs to suppress per-node entries.

Leave-all, deleted-target alias resolution, and reset likewise operate only on
desired Virtuals. The last-rule deletion still removes the desired Virtual; the
publisher then writes empty derived/attachment outputs so xDS snapshots and host
rules converge away.

### Readiness

Direct-listener readiness continues to read `NODE_ATTACHMENTS` and query the
actual agent admin listener list. Sync readiness reads `AGENT_XDS_CONFIG` and
requires the matching derived `SyncTarget` rule to contain a resolved clone IP.
A listener without a resolved sync endpoint is still not ready.

## 8. Compatibility and Failure Semantics

### Old mixed node-agent state

There is deliberately no automatic migration. During manager startup and before
desired-state mutation, an `ENVOY_CONFIG` entry with non-empty `AgentOrigins` is
rejected with an actionable error stating that the old mixed node-agent state is
unsupported and the traffic manager must be reinstalled.

The check must not silently delete or rewrite the old value. A clean base-only
`ENVOY_CONFIG` is valid. Existing sidecar/hostNetwork admission rules are
unchanged.

### Runtime failures

| Scenario | Required behavior |
|---|---|
| Derived Update fails | Return the error, retain the last committed derived state, and retry on a later reconcile |
| Desired YAML is malformed | Abort the attempt without writing derived state |
| Sync clone List fails | Preserve the existing semantics: abort the attempt and retain the last committed state |
| No Ready clone exists | Do not retain a disappeared clone IP; derived sync readiness fails |
| Source pod/Service handling | Preserve current per-source and Service error behavior unless a test demonstrates a state-separation-specific defect |
| Last desired rule is removed | Commit empty derived and attachment lists; xDS clears stale snapshots and agents remove host rules |
| Manager restarts | Rebuild identical derived state from desired plus allocations; old derived state is never an input |
| Identical reconcile input | Produce byte-identical outputs and skip the Update |
| Fresh install without derived key | Start with empty xDS state; readiness passes only after successful derivation and listener creation |

A desired write followed by a publisher crash can temporarily leave desired and
derived at different generations. This is normal eventual consistency: the next
reconcile converges them, and initial attach readiness prevents an unresolved
sync route from being reported ready.

## 9. Integration Test Strategy

Tests must wire real publishers, stores, gRPC services, xDS processing, and fake
Kubernetes clients together. Single-function parser tests are not a substitute.

### 9.1 Writer isolation

1. Add two users' desired rules through `VirtualStore`.
2. Run a real publisher reconcile against fake pods and Services.
3. Assert `ENVOY_CONFIG` is byte-identical to its pre-reconcile value.
4. Assert derived Virtuals, attachments, and allocations contain the expected
   per-node state and stable ports.

### 9.2 DHCP and sync projection

1. Create ordinary and sync-target desired rules.
2. Reconcile and record derived endpoints.
3. Run the real lease `Rules.Sync` callback with a new user TUN IP.
4. Assert only the desired ordinary rule changes.
5. Reconcile and assert the derived ordinary rule receives the new IP while the
   derived sync rule still points to the selected Ready clone.

### 9.3 Conflict and concurrent-writer behavior

Use a fake-client reactor to return one conflict for the publisher's Update.
Between the first and second attempts, change desired state through the real
store. Verify that the successful publisher output contains the latest desired
change and does not restore the removed rule or stale TUN IP.

As with existing fake-client tests, do not claim that the fake client proves real
Kubernetes concurrency. The forced conflict proves the retry/read path; real
cluster CI provides the final integration signal.

### 9.4 Consumer isolation and lifecycle

- Real gRPC status returns desired workloads only, with correct PortMap and
  OwnerID behavior.
- xDS receives derived Virtuals only and clears removed node snapshots.
- Node-agent reconciliation consumes the same committed attachment generation.
- Cover leave, leave-all after target deletion, reset, disconnect, and
  leave→re-proxy.
- Restart the publisher and prove identical allocations and derived output.
- Reconcile twice and assert the second pass performs no Update.
- Seed old mixed `ENVOY_CONFIG` and verify explicit rejection without mutation.

### 9.5 Real-cluster acceptance

Run the complete GitHub matrix: Test (Linux, two-node Linux, Windows no-cluster,
and both macOS suites), Coverage, and Node-Boundary PoC. The two-node test must
continue to prove Service fallback and source-to-clone routing across nodes.
Linux jobs retain the 40-minute limit; exceeding it is a failure requiring log
investigation, not blind reruns.

## 10. Documentation and Rollout

Update `docs/55-node-hosted-agent.md`, `docs/26-sync-mode.md`,
`docs/03-dhcp-ip-allocation.md`, `docs/05-owner-id.md`, and relevant architecture
maps in the same implementation commits. Historical documents remain historical;
current-state sections must name `AGENT_XDS_CONFIG` and the publisher's exclusive
ownership.

Users with the old mixed node-agent ConfigMap must reinstall the traffic manager
and establish fresh attachments. No automatic migration or hidden fallback is
provided.

## 11. Completion Criteria

1. Desired and derived Virtuals use separate keys with the ownership table above.
2. The publisher never writes desired state and retries conflicts by recomputing
   from current input.
3. Status, lifecycle, DHCP, xDS, readiness, and restart paths follow their
   designated state class.
4. Mixed old state fails explicitly without mutation.
5. All new integration tests, the no-cluster suite, integration compilation, and
   the complete real-cluster matrix pass.
