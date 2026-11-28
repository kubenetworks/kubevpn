# Node-Agent Desired/Derived State Separation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split desired base Virtuals from derived per-node Virtuals so the AttachmentPublisher is the sole derived writer and never rewrites desired rules.

**Architecture:** Keep the existing traffic-manager ConfigMap. `ENVOY_CONFIG` becomes desired-only; new `AGENT_XDS_CONFIG` stores derived Virtuals. The publisher atomically writes derived Virtuals, node attachments, and allocations, while xDS/readiness consume derived state and status/lifecycle consumers read desired state.

**Tech Stack:** Go 1.26+, Kubernetes client-go fake clientset and `RetryOnConflict`, `sigs.k8s.io/yaml`, gRPC integration tests, GitHub Actions real-cluster suites.

**Approved spec:** `docs/superpowers/specs/2026-11-28-nodeagent-state-separation-design.md`

## Global Constraints

- Start on `refactor/nodeagent-state-separation` at spec commit `6107d7eb7`; make one logical commit per task.
- `ENVOY_CONFIG` contains only desired base Virtuals. `AGENT_XDS_CONFIG` contains only derived per-node Virtuals.
- AttachmentPublisher must never assign or Update `ENVOY_CONFIG`.
- Publisher derived outputs, attachments, and allocations are committed in one ConfigMap Update. Conflicts retry the complete read/list/compute cycle.
- Old mixed desired/derived `ENVOY_CONFIG` is rejected without mutation and with an actionable reinstall error. Do not migrate it and do not add legacy sidecar compatibility.
- Preserve existing Virtual YAML field names, CLI parameters/output, RPC schemas, OwnerID/DHCP behavior, `SyncTarget`, host DNAT, tunnels, and xDS resource generation.
- `cmd/` remains frozen. Do not edit generated protobuf files.
- Write integration/regression tests before production behavior, run each test and observe the expected failure, then implement the minimum fix.
- Fake-clientset tests must not contact a real cluster. Real-cluster tests remain behind `//go:build integration`.
- Errors are lowercase, wrap with `%w`, and use `errors.Is` for the mixed-state sentinel. Logging uses `plog.G(ctx)`.
- After every task run `go build ./...` and affected `go test` packages. Before every commit run `git diff --check`.

---

### Task 1: Add the Separated-State Contract

**Files:**
- Create: `pkg/controlplane/desired_state.go`
- Create: `pkg/controlplane/desired_state_test.go`
- Modify: `pkg/config/config.go`
- Modify: `pkg/config/config_test.go`
- Modify: `pkg/cluster/manager/manager.go`
- Modify: `pkg/dhcp/dhcp.go`
- Modify: `pkg/dhcp/dhcp_integration_test.go`

**Interfaces:**
- Produces `config.KeyAgentXDS = "AGENT_XDS_CONFIG"` for all later tasks.
- Produces `controlplane.ErrMixedVirtualState` and `controlplane.ValidateDesiredVirtuals([]*Virtual) error`.

- [ ] **Step 1: Write the failing contract tests**

Create `pkg/controlplane/desired_state_test.go`:

```go
package controlplane

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateDesiredVirtuals(t *testing.T) {
	desired := []*Virtual{{Namespace: "default", UID: "deployments.apps.reviews"}}
	if err := ValidateDesiredVirtuals(desired); err != nil {
		t.Fatalf("valid desired state rejected: %v", err)
	}

	mixed := append(desired, &Virtual{
		Namespace: "default", UID: "deployments.apps.reviews.nodes.node-a",
		AgentOrigins: []*AgentOrigin{{PodUID: "pod-a"}},
	})
	err := ValidateDesiredVirtuals(mixed)
	if !errors.Is(err, ErrMixedVirtualState) {
		t.Fatalf("expected ErrMixedVirtualState, got %v", err)
	}
	if !strings.Contains(err.Error(), "reinstall the traffic manager") {
		t.Fatalf("error is not actionable: %v", err)
	}
}
```

Add `KeyAgentXDS` to `TestConfigMapAndKeyConstants` in `pkg/config/config_test.go`:

```go
"KeyAgentXDS": KeyAgentXDS,
```

Add this initialization test to `pkg/dhcp/dhcp_integration_test.go`:

```go
func TestInitDHCPInitializesSeparatedVirtualKeys(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	manager := NewDHCPManager(clientset, "kubevpn")
	if err := manager.InitDHCP(context.Background()); err != nil {
		t.Fatal(err)
	}
	cm, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{config.KeyEnvoy, config.KeyAgentXDS} {
		if value, ok := cm.Data[key]; !ok || value != "" {
			t.Fatalf("key %s was not initialized empty: %q, present=%v", key, value, ok)
		}
	}
}
```

Use the existing imports in that file; add `context`, `metav1`, `fake`, or `config` only if not already present.

- [ ] **Step 2: Run the tests and verify RED**

Run:

```bash
go test ./pkg/controlplane ./pkg/config ./pkg/dhcp -run 'TestValidateDesiredVirtuals|TestConfigMapAndKeyConstants|TestInitDHCPInitializesSeparatedVirtualKeys' -count=1
```

Expected: compilation fails because `ValidateDesiredVirtuals`, `ErrMixedVirtualState`, and `KeyAgentXDS` are undefined.

- [ ] **Step 3: Implement the state contract**

Add to `pkg/config/config.go` beside `KeyEnvoy`:

```go
// KeyAgentXDS stores manager-derived per-node Virtuals for xDS consumers.
// ENVOY_CONFIG remains the desired base Virtual list.
KeyAgentXDS = "AGENT_XDS_CONFIG"
```

Create `pkg/controlplane/desired_state.go`:

```go
package controlplane

import (
	"errors"
	"fmt"
)

var ErrMixedVirtualState = errors.New("mixed desired and derived virtual state")

// ValidateDesiredVirtuals rejects the pre-separation representation rather than
// silently treating runtime per-node projections as desired input.
func ValidateDesiredVirtuals(virtuals []*Virtual) error {
	for _, virtual := range virtuals {
		if virtual != nil && len(virtual.AgentOrigins) != 0 {
			return fmt.Errorf("%w at %s/%s: reinstall the traffic manager", ErrMixedVirtualState, virtual.Namespace, virtual.UID)
		}
	}
	return nil
}
```

Add `config.KeyAgentXDS: ""` to the initial ConfigMap Data in both `pkg/cluster/manager/manager.go` and `pkg/dhcp/dhcp.go`, preserving the existing desired, CIDR, pool, and allocation keys.

- [ ] **Step 4: Verify GREEN and commit**

Run:

```bash
gofmt -w pkg/controlplane/desired_state.go pkg/controlplane/desired_state_test.go pkg/config/config.go pkg/config/config_test.go pkg/cluster/manager/manager.go pkg/dhcp/dhcp.go pkg/dhcp/dhcp_integration_test.go
go test ./pkg/controlplane ./pkg/config ./pkg/cluster/manager ./pkg/dhcp -count=1
go build ./...
git diff --check
git add pkg/controlplane/desired_state.go pkg/controlplane/desired_state_test.go pkg/config/config.go pkg/config/config_test.go pkg/cluster/manager/manager.go pkg/dhcp/dhcp.go pkg/dhcp/dhcp_integration_test.go
git commit -m "refactor: add separated node-agent state contract"
```

Expected: all listed packages pass.

---

### Task 2: Make Publisher and Data Consumers Use Separate Keys

**Files:**
- Modify: `pkg/xds/attachment_publisher.go`
- Modify: `pkg/xds/attachment_publisher_test.go`
- Create: `pkg/xds/state_separation_integration_test.go`
- Modify: `pkg/xds/watcher.go`
- Modify: `pkg/inject/nodeagent_ready.go`
- Modify: `pkg/inject/nodeagent_ready_test.go`
- Modify: `pkg/xds/inject_service_integration_test.go`
- Modify: `pkg/handler/nodeboundary_multinode_integration_test.go`
- Modify: `docs/55-node-hosted-agent.md`
- Modify: `docs/26-sync-mode.md`

**Interfaces:**
- Consumes Task 1's `config.KeyAgentXDS` and `controlplane.ValidateDesiredVirtuals`.
- `AttachmentPublisher.reconcileOnce(context.Context) error` remains the test entry point.
- xDS `NotifyMessage.Content` now carries only `AGENT_XDS_CONFIG` YAML.

- [ ] **Step 1: Convert the publisher lifecycle test to the new contract**

In `TestAttachmentPublisher_PublishesAndRemoves`, save `desiredBefore := cm.Data[config.KeyEnvoy]` before reconcile. Replace the combined-list assertions with:

```go
if got.Data[config.KeyEnvoy] != desiredBefore {
	t.Fatal("publisher mutated desired Virtuals")
}
var derived []*controlplane.Virtual
if err := yaml.Unmarshal([]byte(got.Data[config.KeyAgentXDS]), &derived); err != nil {
	t.Fatal(err)
}
if len(derived) != 1 || derived[0].UID != attachments[0].NodeID || len(derived[0].AgentOrigins) != 1 {
	t.Fatalf("unexpected derived state: %+v", derived)
}
if derived[0].AgentListenerPort != attachments[0].AgentListenerPort {
	t.Fatal("derived admin port differs from attachment port")
}
```

After the second reconcile, compare `KeyAgentXDS`, `KeyNodeAttachments`, and `KeyAgentPortAllocations`; assert `KeyEnvoy == desiredBefore`. After pod deletion, unmarshal `KeyAgentXDS` and require zero derived Virtuals while desired remains unchanged.

Apply the same input/output split to the other publisher tests: desired fixtures stay in `KeyEnvoy`; expectations for per-node Virtuals and clone endpoints read `KeyAgentXDS`; assertions about stored desired rules continue to read `KeyEnvoy`. In `inject_service_integration_test.go`, the pre-seeded derived sync Virtual used by readiness moves to `KeyAgentXDS`; post-inject declarative base assertions remain on `KeyEnvoy`.

- [ ] **Step 2: Add the failing end-to-end publisher/xDS test**

Create `TestIntegration_StateSeparation_PublisherFeedsXDSWithoutMutatingDesired` in `pkg/xds/state_separation_integration_test.go`. The test must:

```go
ctx := context.Background()
cm := &corev1.ConfigMap{
	ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
	Data: map[string]string{config.KeyEnvoy: publisherVirtualsYAML},
}
clientset := fake.NewSimpleClientset(cm, newPublisherTestPod())
publisher := NewAttachmentPublisher(clientset, "kubevpn")
if err := publisher.reconcileOnce(ctx); err != nil {
	t.Fatal(err)
}
stored, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
if err != nil {
	t.Fatal(err)
}
if stored.Data[config.KeyEnvoy] != publisherVirtualsYAML {
	t.Fatal("publisher rewrote desired state")
}
snapshotCache := envoycache.NewSnapshotCache(false, envoycache.IDHash{}, logrus.New())
processor := newProcessor(snapshotCache, logrus.NewEntry(logrus.New()))
if err := processor.ProcessFile(NotifyMessage{Content: stored.Data[config.KeyAgentXDS]}); err != nil {
	t.Fatal(err)
}
nodeID := util.GenEnvoyUID("default", "deployments.apps.reviews.nodes.node-a")
snapshot, err := snapshotCache.GetSnapshot(nodeID)
if err != nil {
	t.Fatalf("derived state did not reach xDS: %v", err)
}
if len(snapshot.GetResources(resource.ListenerType)) == 0 {
	t.Fatal("derived xDS snapshot has no listeners")
}
```

Use imports `envoycache "github.com/envoyproxy/go-control-plane/pkg/cache/v3"`, `resource "github.com/envoyproxy/go-control-plane/pkg/resource/v3"`, `logrus "github.com/sirupsen/logrus"`, and `github.com/wencaiwulue/kubevpn/v2/pkg/util`; alias the cache import consistently in the actual file.

Add `TestAttachmentPublisher_IdenticalReconcileSkipsUpdate`: after one successful reconcile, first require `KeyEnvoy == publisherVirtualsYAML` and non-empty `KeyAgentXDS` so this test is RED on the combined-state implementation. Then install an update reactor counting ConfigMap updates, run reconcile again, and require zero updates. The reactor is:

```go
var updates int32
clientset.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
	atomic.AddInt32(&updates, 1)
	return false, nil, nil
})
```

Use `sync/atomic`, `runtime`, and `k8stesting` imports. This test catches using the allocator's local `changed` boolean as a persisted write condition.

Add `TestAttachmentPublisher_RestartRebuildsSameDerivedState`: reconcile once, save derived YAML and allocations, overwrite `KeyAgentXDS` with malformed-but-parseable stale per-node state, construct a new `AttachmentPublisher` over the same clientset, and reconcile again. Require the saved derived YAML and allocations to be restored exactly and require desired YAML to remain unchanged. This proves the new publisher does not consume the previous derived projection.

- [ ] **Step 3: Run the publisher and readiness tests to verify RED**

Run:

```bash
go test ./pkg/xds ./pkg/inject -run 'StateSeparation|AttachmentPublisher|WaitNodeAgentReady' -count=1
```

Expected: publisher tests fail because derived Virtuals are still appended to `KeyEnvoy`, `KeyAgentXDS` is empty, and the new desired-immutability assertion fails. After implementation, the no-op guard additionally proves that restoring allocations does not cause redundant writes.

- [ ] **Step 4: Split the publisher output**

In `pkg/xds/attachment_publisher.go`:

1. Parse `KeyEnvoy` directly into `virtuals`; call `controlplane.ValidateDesiredVirtuals(virtuals)` and return its error. Delete the `storedVirtuals`/`AgentOrigins` filtering block.
2. Call the allocator with `if _, err = controlplane.EnsureAgentListenerPortsWithState(...); err != nil` and do not use its boolean as an Update condition.
3. Replace `updatedVirtuals` with a sorted derived-only list:

```go
derivedVirtuals := make([]*controlplane.Virtual, 0, len(derivedByNodeID))
for _, nodeID := range nodeIDs {
	derivedVirtuals = append(derivedVirtuals, derivedByNodeID[nodeID])
}
derivedYAML, err := yaml.Marshal(derivedVirtuals)
if err != nil {
	return fmt.Errorf("marshal derived xds config: %w", err)
}
```

4. Replace the write condition and assignments with:

```go
if cm.Data[config.KeyAgentXDS] != string(derivedYAML) ||
	cm.Data[config.KeyNodeAttachments] != string(attachmentsYAML) ||
	cm.Data[config.KeyAgentPortAllocations] != string(allocationsYAML) {
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[config.KeyAgentXDS] = string(derivedYAML)
	cm.Data[config.KeyNodeAttachments] = string(attachmentsYAML)
	cm.Data[config.KeyAgentPortAllocations] = string(allocationsYAML)
	if _, err := p.clientset.CoreV1().ConfigMaps(p.managerNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("write derived node-agent state: %w", err)
	}
}
```

There must be no assignment to `cm.Data[config.KeyEnvoy]` in this file.

- [ ] **Step 5: Switch derived readers**

Change `pkg/xds/watcher.go` line constructing `NotifyMessage` to:

```go
case notifyCh <- NotifyMessage{Content: configMap.Data[config.KeyAgentXDS]}:
```

Change only the sync-derived Virtual read in `pkg/inject/nodeagent_ready.go` from `KeyEnvoy` to `KeyAgentXDS`; keep `NODE_ATTACHMENTS` unchanged. Move the derived fixture in `TestWaitNodeAgentReady_RequiresResolvedSyncTarget` to `KeyAgentXDS`.

Change `waitForDerivedSyncEndpoint` in `pkg/handler/nodeboundary_multinode_integration_test.go` to unmarshal `KeyAgentXDS`. Desired identity assertions remain unchanged.

Update current-state sections of `docs/55-node-hosted-agent.md` and `docs/26-sync-mode.md` to state that base Virtuals use `ENVOY_CONFIG`, derived Virtuals use `AGENT_XDS_CONFIG`, and the publisher never writes desired state.

- [ ] **Step 6: Verify GREEN and commit**

Run:

```bash
gofmt -w pkg/xds/attachment_publisher.go pkg/xds/attachment_publisher_test.go pkg/xds/state_separation_integration_test.go pkg/xds/watcher.go pkg/inject/nodeagent_ready.go pkg/inject/nodeagent_ready_test.go pkg/xds/inject_service_integration_test.go pkg/handler/nodeboundary_multinode_integration_test.go
go test ./pkg/xds ./pkg/inject ./pkg/handler -count=1
go build ./...
git diff --check
rg -n 'cm\.Data\[config\.KeyEnvoy\]\s*=' pkg/xds/attachment_publisher.go
```

Expected: the `rg` command returns no matches; all tests pass.

Commit all Task 2 files:

```bash
git add pkg/xds pkg/inject/nodeagent_ready.go pkg/inject/nodeagent_ready_test.go pkg/handler/nodeboundary_multinode_integration_test.go docs/55-node-hosted-agent.md docs/26-sync-mode.md
git commit -m "refactor: separate derived node-agent xds state"
```

---

### Task 3: Enforce Desired-Only Inputs and Remove Derived Filtering

**Files:**
- Create: `pkg/xds/desired_state.go`
- Create: `pkg/xds/desired_state_test.go`
- Modify: `pkg/xds/tun_config.go`
- Modify: `pkg/inject/virtual_store.go`
- Modify: `pkg/inject/virtual_store_test.go`
- Modify: `pkg/xds/internal/envoy/rules.go`
- Modify: `pkg/xds/internal/envoy/rules_test.go`
- Modify: `pkg/daemon/action/registry_integration_test.go`
- Modify: `pkg/daemon/action/status.go`
- Modify: `pkg/daemon/action/status_test.go`
- Modify: `pkg/xds/inject_service.go`
- Modify: `pkg/xds/derived_virtual_test.go`
- Modify: `docs/03-dhcp-ip-allocation.md`
- Modify: `docs/05-owner-id.md`

**Interfaces:**
- Consumes `controlplane.ValidateDesiredVirtuals` and `ErrMixedVirtualState`.
- Produces `validateDesiredVirtualConfig(context.Context, kubernetes.Interface, string) error` in package `xds` for manager startup.

- [ ] **Step 1: Add failing mixed-state rejection tests**

Create `TestNewTunConfigServerRejectsMixedDesiredState` in `pkg/xds/desired_state_test.go`:

```go
func TestNewTunConfigServerRejectsMixedDesiredState(t *testing.T) {
	mixed := []*controlplane.Virtual{{
		Namespace: "default", UID: "deployments.apps.reviews.nodes.node-a",
		AgentOrigins: []*controlplane.AgentOrigin{{PodUID: "pod-a"}},
	}}
	data, err := yaml.Marshal(mixed)
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
		Data: map[string]string{config.KeyEnvoy: string(data), config.KeyTunIPPool: ""},
	}
	server, err := NewTunConfigServer(context.Background(), fake.NewSimpleClientset(cm), "test-ns")
	if server != nil {
		server.Close()
		t.Fatal("mixed desired state unexpectedly started a server")
	}
	if !errors.Is(err, controlplane.ErrMixedVirtualState) {
		t.Fatalf("expected mixed-state rejection, got %v", err)
	}
	// The validation path must not rewrite the old state.
}
```

To prove non-mutation, retain the fake client in a variable, Get the ConfigMap after the failed startup, and compare `KeyEnvoy` with `string(data)`.

Add `TestVirtualStoreRejectsMixedDesiredState` in `pkg/inject/virtual_store_test.go`. Seed the same mixed YAML, call `store.Mutate` with a callback that sets `called = true`, and require `errors.Is(err, controlplane.ErrMixedVirtualState)`, `called == false`, and unchanged YAML.

Add a lease-rule regression in `pkg/xds/internal/envoy/rules_test.go`: seed mixed YAML, call `Rules.Sync`, and assert the original YAML is unchanged. The existing parse-error/no-write behavior must remain intact.

- [ ] **Step 2: Verify RED**

Run:

```bash
go test ./pkg/xds ./pkg/inject ./pkg/xds/internal/envoy -run 'MixedDesiredState|RejectsMixed' -count=1
```

Expected: the server starts, `VirtualStore.Mutate` invokes its callback, and no mixed-state sentinel is returned.

- [ ] **Step 3: Add startup and write-path validation**

Create `pkg/xds/desired_state.go`:

```go
package xds

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

func validateDesiredVirtualConfig(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read desired virtual config: %w", err)
	}
	var virtuals []*controlplane.Virtual
	if data := cm.Data[config.KeyEnvoy]; data != "" {
		if err := yaml.Unmarshal([]byte(data), &virtuals); err != nil {
			return fmt.Errorf("parse desired virtual config: %w", err)
		}
	}
	return controlplane.ValidateDesiredVirtuals(virtuals)
}
```

Call it after `leases.New` has completed its existing DHCP initialization and transient-error retry, but before returning the server or starting the reaper, publisher, or gRPC service. Close the initialized lease manager on validation failure. Keep the existing initialization order otherwise:

```go
manager, err := leases.New(ctx, clientset, namespace, leases.Callbacks{
	Sync: rules.Sync, Remove: rules.Remove,
})
if err != nil {
	return nil, err
}
if err := validateDesiredVirtualConfig(ctx, clientset, namespace); err != nil {
	manager.Close()
	return nil, err
}
```

This preserves `TestInit_TransientAPIFailure_RecoversAndServes`; validation must not introduce a fail-fast API Get before the existing retry loop.

In `VirtualStore.Mutate`, immediately after YAML unmarshal:

```go
if err = controlplane.ValidateDesiredVirtuals(virtuals); err != nil {
	return err
}
```

In `xds/internal/envoy/Rules.mutateEnvoyVirtuals`, after parsing and before invoking `mutate`:

```go
if parseErr == nil {
	parseErr = controlplane.ValidateDesiredVirtuals(virtuals)
}
```

Keep the existing `onParseErr` path so Sync/Remove log and skip without modifying mixed state.

- [ ] **Step 4: Delete obsolete derived filters**

Delete `if len(virtual.AgentOrigins) != 0 { continue }` from `buildProxyAndSyncStatus` in `pkg/daemon/action/status.go`.

In `pkg/xds/inject_service.go`, remove `len(virtual.AgentOrigins) != 0` from `resolveLeaveNodeID` and `len(v.AgentOrigins) != 0` from `workloadsOwnedBy`. Namespace and OwnerID matching remain unchanged.

Replace `TestBuildProxyAndSyncStatus_SkipsDerivedNodeVirtuals` with `TestBuildProxyAndSyncStatus_UsesDesiredVirtuals`: use one desired Virtual and assert its workload, OwnerID current-device result, and PortMap. Replace `TestWorkloadsOwnedBy_SkipsDerivedNodeVirtuals` with `TestWorkloadsOwnedBy_UsesDesiredState`: seed only desired Virtuals and assert namespace/OwnerID filtering. Do not seed mixed state into consumers that now reject it upstream.

Keep `TestRegistry_RealGRPCStatusAndDuplicateSelection` in `pkg/daemon/action/registry_integration_test.go` as the real gRPC consumer gate. Add an explicit assertion for every returned proxy that `!strings.Contains(p.Workload, ".nodes.")`, while retaining its desired OwnerID, sync, and `8080 -> 9090` PortMap assertions. Combined with the publisher storage test, this proves the gRPC status path never needs the derived key.

Update `docs/03-dhcp-ip-allocation.md` and `docs/05-owner-id.md` so current-state sections identify `ENVOY_CONFIG` as desired-only and `AGENT_XDS_CONFIG` as the xDS projection.

- [ ] **Step 5: Verify GREEN and commit**

Run:

```bash
gofmt -w pkg/xds/desired_state.go pkg/xds/desired_state_test.go pkg/xds/tun_config.go pkg/inject/virtual_store.go pkg/inject/virtual_store_test.go pkg/xds/internal/envoy/rules.go pkg/xds/internal/envoy/rules_test.go pkg/daemon/action/status.go pkg/daemon/action/status_test.go pkg/daemon/action/registry_integration_test.go pkg/xds/inject_service.go pkg/xds/derived_virtual_test.go
go test ./pkg/xds/... ./pkg/inject ./pkg/daemon/action -count=1
go build ./...
git diff --check
git add pkg/xds pkg/inject/virtual_store.go pkg/inject/virtual_store_test.go pkg/daemon/action/status.go pkg/daemon/action/status_test.go pkg/daemon/action/registry_integration_test.go docs/03-dhcp-ip-allocation.md docs/05-owner-id.md
git commit -m "refactor: enforce desired-only virtual state"
```

Expected: all affected packages pass.

---

### Task 4: Retry Publisher Conflicts by Recomputing Latest Desired State

**Files:**
- Modify: `pkg/xds/attachment_publisher.go`
- Modify: `pkg/xds/state_separation_integration_test.go`
- Modify: `pkg/xds/attachment_publisher_test.go` only if an existing fixture must expose the attempt helper
- Modify: `docs/55-node-hosted-agent.md`

**Interfaces:**
- Preserves `reconcileOnce(context.Context) error`.
- Adds unexported `reconcileAttempt(context.Context) error`; no other package depends on it.

- [ ] **Step 1: Write the failing forced-conflict integration test**

Add `TestAttachmentPublisher_ConflictRecomputesLatestDesired` to `pkg/xds/state_separation_integration_test.go`. Seed the standard desired fixture and `newPublisherTestPod()`, then install this reactor before calling `reconcileOnce`:

```go
gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
var attempts int32
clientset.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
	if atomic.AddInt32(&attempts, 1) != 1 {
		return false, nil, nil
	}
	current, err := clientset.Tracker().Get(gvr, "kubevpn", config.ConfigMapPodTrafficManager)
	if err != nil {
		return true, nil, err
	}
	latest := current.(*corev1.ConfigMap).DeepCopy()
	latest.Data[config.KeyEnvoy] = strings.Replace(
		latest.Data[config.KeyEnvoy], "198.18.0.1", "198.18.0.99", 1,
	)
	if err := clientset.Tracker().Update(gvr, latest, "kubevpn"); err != nil {
		return true, nil, err
	}
	return true, nil, apierrors.NewConflict(
		schema.GroupResource{Resource: "configmaps"},
		config.ConfigMapPodTrafficManager,
		fmt.Errorf("injected conflict"),
	)
})
```

Call `publisher.reconcileOnce(ctx)`. Require success, `attempts == 2`, desired YAML containing `198.18.0.99`, and a derived rule containing `198.18.0.99`. If a retry reuses the first attempt's desired object, the derived rule remains `198.18.0.1` and the test fails.

Add the final desired/derived readback with:

```go
stored, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
if err != nil {
	t.Fatal(err)
}
var desired, derived []*controlplane.Virtual
if err := yaml.Unmarshal([]byte(stored.Data[config.KeyEnvoy]), &desired); err != nil {
	t.Fatal(err)
}
if err := yaml.Unmarshal([]byte(stored.Data[config.KeyAgentXDS]), &derived); err != nil {
	t.Fatal(err)
}
if desired[0].Rules[0].LocalTunIPv4 != "198.18.0.99" || derived[0].Rules[0].LocalTunIPv4 != "198.18.0.99" {
	t.Fatalf("publisher did not recompute latest desired: desired=%+v derived=%+v", desired, derived)
}
```

- [ ] **Step 2: Verify RED**

Run:

```bash
go test ./pkg/xds -run '^TestAttachmentPublisher_ConflictRecomputesLatestDesired$' -count=1
```

Expected: `reconcileOnce` returns the injected conflict after one update attempt.

- [ ] **Step 3: Implement whole-attempt conflict retry**

Rename the existing method body to `reconcileAttempt` and add this wrapper in `pkg/xds/attachment_publisher.go`:

```go
func (p *AttachmentPublisher) reconcileOnce(ctx context.Context) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		return p.reconcileAttempt(ctx)
	})
}
```

Import `k8s.io/client-go/util/retry`. The entire ConfigMap Get, desired parse, workload/Service/clone List operations, allocation, and Update must remain in `reconcileAttempt`; do not narrow retry to only the Update call.

Document in `docs/55-node-hosted-agent.md` that conflicts restart derivation from the newest desired state and ResourceVersion.

- [ ] **Step 4: Verify GREEN and the complete local loop**

Run:

```bash
gofmt -w pkg/xds/attachment_publisher.go pkg/xds/state_separation_integration_test.go
go test ./pkg/xds -run 'StateSeparation|AttachmentPublisher' -count=1
go test ./pkg/xds/... ./pkg/inject ./pkg/daemon/action ./pkg/handler -count=1
go build ./...
go vet ./pkg/...
go test ./pkg/...
make ut-compile
git diff --check
```

Expected: the conflict test performs two attempts and all packages pass.

Commit:

```bash
git add pkg/xds/attachment_publisher.go pkg/xds/state_separation_integration_test.go pkg/xds/attachment_publisher_test.go docs/55-node-hosted-agent.md
git commit -m "fix: recompute node-agent state after config conflicts"
```

If `attachment_publisher_test.go` was not changed, omit it from `git add` rather than committing a no-op path.

---

## Final Verification Gate

- [ ] Search for forbidden ownership regressions:

```bash
rg -n 'cm\.Data\[config\.KeyEnvoy\]\s*=' pkg/xds/attachment_publisher.go
rg -n 'KeyAgentXDS|AGENT_XDS_CONFIG' pkg docs/55-node-hosted-agent.md docs/26-sync-mode.md docs/03-dhcp-ip-allocation.md docs/05-owner-id.md
rg -n 'legacy-proxy|AttachModeSidecar|AttachModeLegacySidecar' pkg cmd || true
```

Expected: no publisher desired write; the new key has producer/consumer/test/documentation coverage; no removed sidecar implementation reappears.

- [ ] Run the complete local verification loop again:

```bash
go build ./...
go vet ./pkg/...
go test ./pkg/...
make ut-compile
```

- [ ] Push the branch and run the full GitHub matrix: Test (including `linux-multinode`), Coverage, and Node-Boundary PoC. Do not use a targeted suite as the final result.

- [ ] Verify real-cluster lifecycle: ordinary proxy/run, sync full/header and clone movement, Service portmap/fallback, leave/re-proxy, manager/agent/target restart, unchanged workload generation/Pod UID, and two-node cross-node routes.

- [ ] Treat Linux jobs over 40 minutes as failed and inspect logs immediately. Do not mark the project complete until the full matrix is green.
