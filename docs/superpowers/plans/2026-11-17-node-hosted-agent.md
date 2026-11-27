# Node-hosted Traffic Agent Implementation Plan (docs/55)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship docs/55 — `kubevpn proxy` without patching workloads: the traffic-manager creates a privileged, node-pinned Job per target pod that attaches to the pod's network namespace from outside (docs/55 §3), reusing the entire mesh routing stack (iptables → `:15006` → xDS → TUN IP).

**Architecture:** Three pieces. (1) A new `kubevpn agent` command + `pkg/agent`: locate the target pod's netns by scanning `/proc` for the pod UID's cgroup, `setns` into it, then `syscall.Exec` the real payload (VPN bootstrap or envoy). (2) A third injector strategy `nodeAgentInjector` in `pkg/inject` that writes the same ENVOY_CONFIG rules as mesh mode and then ensures one deterministic `batch/v1.Job` per target pod in the manager namespace. (3) A reconciler in `pkg/xds` that keeps Jobs aligned with pod rescheduling. Attach-mode is cluster-level config (env var on the traffic-manager + Helm value) — no existing CLI command changes.

**Tech Stack:** Go 1.26.3, `golang.org/x/sys/unix` (already in go.mod, v0.43.0 — measured), `k8s.io/api/batch/v1`, `k8s.io/client-go` informers, fake.NewSimpleClientset for tests.

## Global Constraints

- Module `github.com/wencaiwulue/kubevpn/v2`; `CGO_ENABLED=0`; no `-race`.
- `cmd/` frozen for existing commands — new subcommand `kubevpn agent` is allowed (new file + registration).
- Generated files off-limits (`pkg/daemon/rpc/*.pb.go`).
- Verification loop before every commit: `go build ./...` → `go vet ./pkg/...` → `go test ./pkg/...`.
- Every refactor includes integration tests against `fake.NewSimpleClientset()` structured as multi-phase stories (docs/55 §10).
- RBAC fact (measured): `charts/kubevpn/templates/clusterrole.yaml` already grants wildcard apiGroups/resources with create/delete verbs — **no Helm RBAC change needed**. The lazy namespaced path (`genProxyRole`, workload namespaces) does NOT cover the manager namespace, so the manager's own Role needs `batch/jobs` rules (Task 6).
- Honest limitation carried from docs/55 §8: mode refuses cleanly on PSS-restricted clusters; GKE Autopilot unsupported; `hostUsers: false` unsupported (detect pod `spec.hostUsers == false` and error).

---

### Task 1: `AttachMode` on InjectOptions + three-way strategy selection

**Files:**
- Modify: `pkg/inject/injector.go:31-60`
- Modify: `pkg/xds/inject_service.go:64-128`
- Test: `pkg/inject/injector_attach_test.go`

**Interfaces:**
- Consumes: existing `NewInjector(opts InjectOptions) Injector`, existing Service-target detection inside its body.
- Produces: `InjectOptions.AttachMode string` (values `config.AttachModeSidecar` / `config.AttachModeNodeAgent`), and `config` constants. Task 3/4 depend on `AttachModeNodeAgent`; Task 7 reads it server-side.

- [ ] **Step 1: Add constants in `pkg/config/config.go`**

```go
// Attach modes for proxy injection (docs/55-node-hosted-agent.md). Cluster-level
// configuration: the traffic-manager decides, the client sends no mode.
const (
	AttachModeSidecar   = "sidecar"
	AttachModeNodeAgent = "node-agent"
)
```

- [ ] **Step 2: Write the failing test**

Create `pkg/inject/injector_attach_test.go`:

```go
package inject

import (
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestNewInjectorSelectsNodeAgentForNonService(t *testing.T) {
	opts := minimalInjectOptionsForTest() // helper below
	opts.AttachMode = config.AttachModeNodeAgent
	inj := NewInjector(opts)
	if _, ok := inj.(*nodeAgentInjector); !ok {
		t.Fatalf("want nodeAgentInjector for non-Service + node-agent mode, got %T", inj)
	}
}

func TestNewInjectorSelectsMeshByDefault(t *testing.T) {
	opts := minimalInjectOptionsForTest()
	inj := NewInjector(opts)
	if _, ok := inj.(*meshInjector); !ok {
		t.Fatalf("want meshInjector by default, got %T", inj)
	}
}

func TestNewInjectorSelectsFargateForServiceEvenInNodeAgentMode(t *testing.T) {
	opts := minimalInjectOptionsForTest()
	opts.AttachMode = config.AttachModeNodeAgent
	opts.Object = serviceObjectForTest() // helper: *resource.Info whose Object is a v1.Service
	inj := NewInjector(opts)
	if _, ok := inj.(*fargateInjector); !ok {
		t.Fatalf("Service target must keep fargateInjector regardless of mode, got %T", inj)
	}
}

// minimalInjectOptionsForTest builds the minimal non-Service InjectOptions the
// factory needs. Reuse the exact construction pattern of an existing inject
// test that builds a Deployment controller (grep `NewInjector(` in
// pkg/inject/*_test.go and copy its Deployment/resource.Info fixtures; keep
// this helper under 20 lines by calling that fixture builder).
func minimalInjectOptionsForTest() InjectOptions {
	// replaced in Step 3 by the copied fixture — see Step 3
	return InjectOptions{}
}

func serviceObjectForTest() *resource.Info {
	svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"}}
	// mirror the resource.Info construction used by the copied fixture
	return &resource.Info{Object: svc, Mapping: &meta.RESTMapping{Resource: schema.GroupVersionResource{Resource: "services"}}, Namespace: "ns", Name: "svc"}
}
```

The two fixture helpers must be completed by copying the existing fixture pattern from `pkg/inject/*_test.go` (an existing test already builds a Deployment `*resource.Info` for `NewInjector` — locate it with `grep -rn "NewInjector(" pkg/inject/*_test.go` and reuse its builder). The assertion logic above is the contract; the fixture copy is mechanical.

- [ ] **Step 3: Implement**

In `pkg/config/config.go` add the constants (Step 1). In `pkg/inject/injector.go` add `AttachMode string` to `InjectOptions` and extend `NewInjector`:

```go
// NewInjector returns the appropriate Injector for the given workload and options.
//   - Service targets use Fargate mode (SSH + Envoy) — attach-mode never applies.
//   - Non-Service targets use the cluster-configured attach mode: node-hosted
//     agent Jobs (docs/55) or mesh sidecars (docs/17). Empty headers remain
//     full-proxy inside either mode.
func NewInjector(opts InjectOptions) Injector {
	// <keep the existing Service-target condition exactly as it is today>
	if isServiceTarget(opts.Object) {
		return &fargateInjector{opts}
	}
	if opts.AttachMode == config.AttachModeNodeAgent {
		return &nodeAgentInjector{opts: opts}
	}
	return &meshInjector{opts}
}
```

Rename the existing inline Service check into `isServiceTarget(object *resource.Info) bool` and call it from the factory (pure extraction, no behavior change). Create `pkg/inject/nodeagent.go` with the empty strategy shell (filled in Task 4):

```go
package inject

// nodeAgentInjector attaches to a workload without patching it: writes the
// same ENVOY_CONFIG rules as mesh mode, then ensures one privileged Job per
// target pod in the manager namespace (docs/55 §3-§5).
type nodeAgentInjector struct {
	opts InjectOptions
}

func (n *nodeAgentInjector) Inject(ctx context.Context) error {
	return nil // implemented in Task 4
}
```

- [ ] **Step 4: Wire server-side mode from env**

In `pkg/xds/inject_service.go`, where `InjectOptions` is built (lines 64-128), add:

```go
AttachMode: attachModeFromEnv(),
```

and in `pkg/xds/tun_config.go`:

```go
// attachModeFromEnv reads the cluster-level proxy attach mode. Default is the
// sidecar injector; the value is set by the Helm chart / genDeploySpec (Task 6).
func attachModeFromEnv() string {
	switch os.Getenv("KUBEVPN_PROXY_ATTACH_MODE") {
	case config.AttachModeNodeAgent:
		return config.AttachModeNodeAgent
	default:
		return config.AttachModeSidecar
	}
}
```

- [ ] **Step 5: Run tests, verify loop, commit**

Run: `go build ./... && go vet ./pkg/... && go test ./pkg/inject/... ./pkg/xds/... -v`
Expected: PASS.

```bash
git add pkg/config/config.go pkg/inject/ pkg/xds/
git commit -m "feat(inject): attach-mode strategy selection (node-agent opt-in)"
```

---

### Task 2: `pkg/agent` — netns discovery + enter (testable, no cluster)

**Files:**
- Create: `pkg/agent/netns.go`
- Test: `pkg/agent/netns_test.go`

**Interfaces:**
- Consumes: `golang.org/x/sys/unix` (already in go.mod).
- Produces: `func FindNetnsPID(procRoot, podUID string) (int, error)` and `func EnterNetns(procRoot string, pid int) error` — Task 3 calls both.

- [ ] **Step 1: Write the failing test**

Create `pkg/agent/netns_test.go`:

```go
package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// buildFakeProc fabricates a /proc tree: numeric dirs with cgroup files.
func buildFakeProc(t *testing.T, entries map[int]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, cg := range entries {
		dir := filepath.Join(root, itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestFindNetnsPIDMatchesPodUIDInCgroupV1AndV2(t *testing.T) {
	uid := "6f2a-4c1e-9a0b"
	root := buildFakeProc(t, map[int]string{
		1:    "12:cpuset:/docker/other",
		3482: "0::/kubepods/pod" + uid + "/9be2a1f...",
		3501: "10:cpu,cpuacct:/kubepods/burstable/pod" + uid + "/cc11...",
	})
	pid, err := FindNetnsPID(root, uid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pid != 3482 {
		t.Fatalf("want lowest matching PID 3482, got %d", pid)
	}
}

func TestFindNetnsPIDErrorsWhenAbsent(t *testing.T) {
	root := buildFakeProc(t, map[int]string{1: "0::/"})
	if _, err := FindNetnsPID(root, "nope"); err == nil {
		t.Fatal("expected error when no process matches")
	}
}
```

(`EnterNetns` performs a real `setns` — not unit-testable portably; it is covered by the real-cluster validation in Task 8. Keep it thin.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./pkg/agent/ -v`
Expected: FAIL — package `agent` does not exist.

- [ ] **Step 3: Implement**

Create `pkg/agent/netns.go`:

```go
// Package agent implements the node-hosted traffic agent (docs/55): locating a
// target pod's network namespace from outside the pod and entering it.
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// FindNetnsPID scans procRoot for the first process whose cgroup path contains
// podUID (cgroup v1 and v2 both embed the pod UID in the kubelet's cgroupfs
// layout: .../kubepods/pod<pod-uid>/...). procRoot is the mounted host /proc.
func FindNetnsPID(procRoot, podUID string) (int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, fmt.Errorf("read proc root %s: %w", procRoot, err)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // skip non-numeric entries (cgroups, mounts, ...)
		}
		data, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "cgroup"))
		if err != nil {
			continue // kernel threads / racing exits
		}
		if strings.Contains(string(data), podUID) {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("cannot find netns for pod uid %s under %s", podUID, procRoot)
}

// EnterNetns moves the calling thread into the target PID's network namespace.
// The caller must have runtime.LockOSThread held (the kubevpn agent command
// does) and typically follows this with syscall.Exec so the exec'd payload
// runs inside the pod netns.
func EnterNetns(procRoot string, pid int) error {
	runtime.LockOSThread()
	fd, err := unix.Open(fmt.Sprintf("%s/%d/ns/net", procRoot, pid), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open netns of pid %d: %w", pid, err)
	}
	defer unix.Close(fd)
	if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
		return fmt.Errorf("setns to pid %d netns: %w", pid, err)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass, commit**

Run: `go build ./... && go test ./pkg/agent/ -v`
Expected: PASS.

```bash
git add pkg/agent/
git commit -m "feat(agent): locate and enter a pod's netns by pod UID /proc scan"
```

---

### Task 3: `kubevpn agent` command (hidden)

**Files:**
- Create: `cmd/kubevpn/cmds/agent.go`
- Create: `pkg/agent/exec.go`
- Modify: `cmd/kubevpn/cmds/root.go` (one registration line)
- Test: `pkg/agent/exec_test.go`

**Interfaces:**
- Consumes: `FindNetnsPID`, `EnterNetns` (Task 2).
- Produces: `kubevpn agent --pod-uid <uid> --proc-root /host/proc -- <argv...>` — Task 4's Job spec invokes it with `--exec` payloads.

- [ ] **Step 1: Write the failing test for the exec helper**

Create `pkg/agent/exec_test.go`:

```go
package agent

import (
	"strings"
	"testing"
)

func TestExecArgsValidated(t *testing.T) {
	if err := ValidateExecArgs(nil); err == nil || !strings.Contains(err.Error(), "exec") {
		t.Fatalf("empty argv must error, got %v", err)
	}
	if err := ValidateExecArgs([]string{"ok"}); err != nil {
		t.Fatalf("non-empty argv must pass, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails** (`undefined: ValidateExecArgs`), then implement.

Create `pkg/agent/exec.go`:

```go
package agent

import (
	"fmt"
	"os"
	"syscall"
)

// ValidateExecArgs guards the exec payload.
func ValidateExecArgs(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return fmt.Errorf("exec payload required after --")
	}
	return nil
}

// Exec replaces the current process with argv after entering the netns. Used
// by the kubevpn agent command; never returns on success.
func Exec(argv []string) error {
	if err := ValidateExecArgs(argv); err != nil {
		return err
	}
	return syscall.Exec(argv[0], argv, os.Environ())
}
```

- [ ] **Step 3: Write the command**

Create `cmd/kubevpn/cmds/agent.go`:

```go
package cmds

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	pkgagent "github.com/wencaiwulue/kubevpn/v2/pkg/agent"
)

// CmdAgent is the node-hosted traffic agent entrypoint (docs/55). Hidden: it
// is invoked by kubevpn-nodeagent Jobs, not by users.
func CmdAgent(f cmdutil.Factory) *cobra.Command {
	var podUID, procRoot string
	cmd := &cobra.Command{
		Use:    "agent --pod-uid <uid> -- <argv...>",
		Short:  "Node-hosted traffic agent: enter a pod's netns and exec a payload",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if podUID == "" {
				return fmt.Errorf("--pod-uid is required")
			}
			pid, err := pkgagent.FindNetnsPID(procRoot, podUID)
			if err != nil {
				return err
			}
			if err := pkgagent.EnterNetns(procRoot, pid); err != nil {
				return err
			}
			return pkgagent.Exec(args)
		},
	}
	cmd.Flags().StringVar(&podUID, "pod-uid", "", "UID of the target pod")
	cmd.Flags().StringVar(&procRoot, "proc-root", "/host/proc", "Path to the mounted host /proc")
	_ = genericclioptions.NewConfigFlags(true)
	_ = f
	return cmd
}
```

Match the import/flag style of neighboring command files; drop the two `_ =` filler lines if the file style ends up not needing `f`/`genericclioptions` (the factory parameter must remain in the signature for root.go registration).

Register in `cmd/kubevpn/cmds/root.go` in the hidden "Server Commands" group, after `CmdOnce(factory),`:

```go
				CmdAgent(factory),
```

- [ ] **Step 4: Verify, commit**

Run: `go build ./... && go run ./cmd/kubevpn agent --help`
Expected: build PASS; help shows the hidden command; `git diff --stat -- cmd/` shows only `agent.go` (new) and `root.go` (one line).

```bash
git add cmd/kubevpn/cmds/agent.go cmd/kubevpn/cmds/root.go pkg/agent/
git commit -m "feat: hidden kubevpn agent command (netns enter + exec payload)"
```

---

### Task 4: `genAgentJobSpec` + `nodeAgentInjector.Inject` + integration tests

**Files:**
- Create: `pkg/inject/nodeagent_job.go`
- Modify: `pkg/inject/nodeagent.go` (fill `Inject`)
- Test: `pkg/inject/nodeagent_integration_test.go` (no `integration` tag — pure fake clientset; rename if the repo's tag convention requires: use `*_test.go` without cluster access)

**Interfaces:**
- Consumes: `addEnvoyConfig`, `collectPorts`, `InjectOptions`, `config.AttachModeNodeAgent`.
- Produces:
  - `func agentJobName(uid types.UID) string` → `kubevpn-nodeagent-<podUID>`;
  - `func genAgentJobSpec(opts InjectOptions, pod *v1.Pod) *batchv1.Job`;
  - `func listControllerPods(ctx, cs, controller) (*v1.PodList, error)`;
  - `func ensureAgentJob(ctx, cs, opts, pod) error` (create-or-skip on AlreadyExists).

- [ ] **Step 1: Write the integration test (scenarios 1, 2, 5, plus hostUsers guard)**

Create `pkg/inject/nodeagent_test.go`:

```go
package inject

import (
	"context"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// newFakeWorld builds: one Deployment controller, one pod on node-a, and the
// manager ConfigMap backing addEnvoyConfig. Copy the Deployment/resource.Info
// fixture construction from existing pkg/inject tests (same helper as Task 1).
func newFakeWorld(t *testing.T) (*fake.Clientset, InjectOptions, *v1.Pod) {
	t.Helper()
	// ...copy fixture builder; returns opts with AttachMode=node-agent,
	// ManagerNamespace "kubevpn", OwnerID "owner-1", plus a running pod:
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "ns", UID: "pod-uid-1"},
		Spec: v1.PodSpec{NodeName: "node-a"}}
	return cs, opts, pod
}

func TestNodeAgentInjectWritesRulesAndCreatesPinnedJob(t *testing.T) {
	cs, opts, pod := newFakeWorld(t)
	// seed the pod into the fake clientset so listControllerPods finds it
	_, _ = cs.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod.DeepCopy(), metav1.CreateOptions{})

	inj := NewInjector(opts)
	if err := inj.Inject(context.Background()); err != nil {
		t.Fatalf("inject: %v", err)
	}

	job, err := cs.BatchV1().Jobs(opts.ManagerNamespace).Get(context.Background(), agentJobName(pod.UID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("agent job not created: %v", err)
	}
	if job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != "node-a" {
		t.Fatalf("job not pinned to target pod's node: %+v", job.Spec.Template.Spec.NodeSelector)
	}
	if !job.Spec.Template.Spec.HostPID {
		t.Fatal("job must set hostPID")
	}
	priv := false
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
			priv = true
		}
	}
	if !priv {
		t.Fatal("agent container must be privileged")
	}
	if job.Labels["kubevpn/owner-id"] != opts.OwnerID || job.Labels["kubevpn/pod-uid"] != string(pod.UID) {
		t.Fatalf("job labels missing: %+v", job.Labels)
	}
	// rule written to ConfigMap: assert the ENVOY_CONFIG key contains the NodeID
	cm, _ := cs.CoreV1().ConfigMaps(opts.ManagerNamespace).Get(context.Background(), "kubevpn-config", metav1.GetOptions{})
	if !strings.Contains(cm.Data["ENVOY_CONFIG"], opts.NodeID) {
		t.Fatalf("envoy rule not written for nodeID %s", opts.NodeID)
	}
}

func TestNodeAgentInjectIsIdempotentPerOwner(t *testing.T) {
	cs, opts, pod := newFakeWorld(t)
	_, _ = cs.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod.DeepCopy(), metav1.CreateOptions{})
	_ = NewInjector(opts).Inject(context.Background())
	// second user, different headers, same workload → job must NOT duplicate
	opts.OwnerID = "owner-2"
	opts.Headers = map[string]string{"X-User": "b"}
	if err := NewInjector(opts).Inject(context.Background()); err != nil {
		t.Fatalf("second inject: %v", err)
	}
	jobs, _ := cs.BatchV1().Jobs(opts.ManagerNamespace).List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 {
		t.Fatalf("want exactly 1 agent job after two owners, got %d", len(jobs.Items))
	}
}

func TestNodeAgentInjectRefusesUserNamespacedPods(t *testing.T) {
	cs, opts, pod := newFakeWorld(t)
	pod.Spec.HostUsers = ptr.To(false)
	_, _ = cs.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod.DeepCopy(), metav1.CreateOptions{})
	err := NewInjector(opts).Inject(context.Background())
	if err == nil || !strings.Contains(err.Error(), "hostUsers") {
		t.Fatalf("expected hostUsers refusal, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails** (`undefined: agentJobName`, `nodeAgentInjector.Inject` no-op), then implement.

- [ ] **Step 3: Implement the Job generator + injector**

Create `pkg/inject/nodeagent_job.go`:

```go
package inject

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// agentJobName is deterministic per target pod, making create-or-skip
// idempotent for reconcile (Task 7).
func agentJobName(uid types.UID) string {
	return fmt.Sprintf("kubevpn-nodeagent-%s", uid)
}

// genAgentJobSpec builds the privileged, node-pinned agent Job (docs/55 §4).
// Two containers mirror the mesh sidecar pair; both enter the pod netns via
// `kubevpn agent` before exec'ing their payload.
func genAgentJobSpec(opts InjectOptions, pod *v1.Pod) *batchv1.Job {
	privileged := true
	tlsEnv := []v1.EnvVar{}
	if opts.Secret != nil {
		tlsEnv = []v1.EnvVar{
			{Name: "tls_crt", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{Name: opts.Secret.Name}, Key: "tls_crt"}}},
			{Name: "tls_key", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{Name: opts.Secret.Name}, Key: "tls_key"}}},
			{Name: "tls_server_name", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{Name: opts.Secret.Name}, Key: "tls_server_name"}}},
		}
	}
	commonEnv := append([]v1.EnvVar{
		{Name: "CIDR4", Value: config.CIDR.String()},
		{Name: "CIDR6", Value: config.CIDR6.String()},
		{Name: "TrafficManagerService", Value: fmt.Sprintf("kubevpn-traffic-manager.%s", opts.ManagerNamespace)},
		{Name: "POD_UID", Value: string(pod.UID)},
	}, tlsEnv...)

	vpnScript := `
echo 1 > /proc/sys/net/ipv4/ip_forward
echo 0 > /proc/sys/net/ipv6/conf/all/disable_ipv6
echo 1 > /proc/sys/net/ipv6/conf/all/forwarding
echo 0 > /proc/sys/net/ipv4/tcp_timestamps
for b in iptables-nft iptables-legacy; do "$b" -t nat -L &>/dev/null && update-alternatives --set iptables /usr/sbin/$b && update-alternatives --set ip6tables /usr/sbin/${b/iptables/ip6tables} && break; done
iptables -P INPUT ACCEPT; ip6tables -P INPUT ACCEPT
iptables -P FORWARD ACCEPT; ip6tables -P FORWARD ACCEPT
iptables -t nat -A PREROUTING -p tcp ! -s 127.0.0.1 ! -d ${CIDR4} -j DNAT --to :15006
ip6tables -t nat -A PREROUTING -p tcp ! -s 0:0:0:0:0:0:0:1 ! -d ${CIDR6} -j DNAT --to :15006
exec kubevpn server --debug -l "tun:/tcp://${TrafficManagerService}:10801?route=${CIDR4}"
`
	// NOTE: the envoy payload renders the same embedded bootstrap template the
	// mesh envoy sidecar uses; locate the render helper used by
	// AddVPNAndEnvoyContainer (`grep -rn "envoy.yaml" pkg/inject/`) and reuse it
	// inside the agent's envoy payload. The script below assumes the rendered
	// file lands at /etc/kubevpn/envoy.yaml.
	envoyScript := `
kubevpn agent-envoy-config --manager ${TrafficManagerService} > /etc/kubevpn/envoy.yaml
exec envoy -c /etc/kubevpn/envoy.yaml --log-level warning
`

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentJobName(pod.UID),
			Namespace: opts.ManagerNamespace,
			Labels: map[string]string{
				"kubevpn/owner-id": opts.OwnerID,
				"kubevpn/pod-uid":  string(pod.UID),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](3),
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"kubevpn/owner-id": opts.OwnerID,
					"kubevpn/pod-uid":  string(pod.UID),
				}},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyOnFailure,
					NodeSelector:  map[string]string{"kubernetes.io/hostname": pod.Spec.NodeName},
					HostPID:       true,
					Volumes: []v1.Volume{{
						Name: "host-proc",
						VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{
							Path: "/proc", Type: ptr.To(v1.HostPathDirectory),
						}},
					}},
					Containers: []v1.Container{
						{
							Name:    "vpn",
							Image:   opts.Image,
							Command: []string{"/bin/bash", "-c"},
							Args:    []string{fmt.Sprintf("kubevpn agent --pod-uid %s --proc-root /host/proc -- /bin/bash -c %q", pod.UID, vpnScript)},
							Env:     commonEnv,
							SecurityContext: &v1.SecurityContext{
								Privileged: &privileged,
							},
							VolumeMounts: []v1.VolumeMount{{Name: "host-proc", MountPath: "/host/proc", ReadOnly: true}},
						},
						{
							Name:    "envoy",
							Image:   opts.Image,
							Command: []string{"/bin/bash", "-c"},
							Args:    []string{fmt.Sprintf("kubevpn agent --pod-uid %s --proc-root /host/proc -- /bin/bash -c %q", pod.UID, envoyScript)},
							Env:     commonEnv,
							SecurityContext: &v1.SecurityContext{
								Privileged: &privileged,
							},
							VolumeMounts: []v1.VolumeMount{{Name: "host-proc", MountPath: "/host/proc", ReadOnly: true}},
						},
					},
				},
			},
		},
	}
	return job
}
```

Verify at implementation time (docs/55 §12 assumption): `grep -rn "envoy" build/Dockerfile` and `grep -rn "AddEnvoyContainer\|envoy.yaml" pkg/inject/container.go` — confirm the image contains the `envoy` binary and that the manager address/port used by the bootstrap template matches `${TrafficManagerService}`. If the envoy sidecar's config file path differs, use that path verbatim.

Create the payload helper `kubevpn agent-envoy-config` inside `cmd/kubevpn/cmds/agent.go` (extend the command file from Task 3 with a second hidden command or a subcommand):

```go
// CmdAgentEnvoyConfig prints the envoy bootstrap config to stdout — used by
// the node-hosted agent's envoy container before exec'ing envoy.
func CmdAgentEnvoyConfig(f cmdutil.Factory) *cobra.Command {
	var managerAddr string
	cmd := &cobra.Command{
		Use:    "agent-envoy-config",
		Short:  "Print the envoy xDS bootstrap config for the node-hosted agent",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := inject.RenderEnvoyBootstrap(cmd.Context(), managerAddr)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(b)
			return err
		},
	}
	cmd.Flags().StringVar(&managerAddr, "manager", "", "traffic-manager address (host:port)")
	_ = cmd.MarkFlagRequired("manager")
	return cmd
}
```

`inject.RenderEnvoyBootstrap(ctx, addr)` is a small exported wrapper over the existing embedded template rendering used by the envoy sidecar — extract it from the container.go template code path (locate with the grep above; the wrapper renders the mesh template with the manager address and returns bytes).

Fill `pkg/inject/nodeagent.go`:

```go
func (n *nodeAgentInjector) Inject(ctx context.Context) error {
	o := n.opts
	u := o.Controller.Object.(*unstructured.Unstructured)

	templateSpec, _, err := clusterworkload.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}
	ports, portmap := collectPorts(templateSpec, o.PortMaps)
	// Rules are identical to mesh mode — same xDS contract, same OwnerID merge.
	if err := addEnvoyConfig(ctx, o.Clientset.CoreV1().ConfigMaps(o.ManagerNamespace), envoyRuleSpec{
		Namespace:    o.Controller.Namespace,
		NodeID:       o.NodeID,
		LocalTunIPv4: o.LocalTunIPv4,
		LocalTunIPv6: o.LocalTunIPv6,
		Headers:      o.Headers,
		PortMap:      portmap,
		Ports:        ports,
		OwnerID:      o.OwnerID,
	}); err != nil {
		return fmt.Errorf("add envoy config: %w", err)
	}

	pods, err := listControllerPods(ctx, o.Clientset, o.Controller)
	if err != nil {
		return fmt.Errorf("list controller pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.HostUsers != nil && !*pod.Spec.HostUsers {
			return fmt.Errorf("pod %s/%s uses hostUsers: false — node-agent attach is unsupported (docs/55 §8)", pod.Namespace, pod.Name)
		}
		if err := ensureAgentJob(ctx, o.Clientset, o, pod); err != nil {
			return fmt.Errorf("ensure agent job for pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}
	return nil
}

// listControllerPods finds the running pods of the workload via its template
// labels (pods carry the template labels; selectors alone can be absent on
// some controller kinds).
func listControllerPods(ctx context.Context, cs kubernetes.Interface, controller *resource.Info) (*v1.PodList, error) {
	u, ok := controller.Object.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("controller is not unstructured: %T", controller.Object)
	}
	labels, found, err := unstructured.NestedStringMap(u.Object, "spec", "template", "metadata", "labels")
	if err != nil || !found || len(labels) == 0 {
		return nil, fmt.Errorf("workload %s/%s has no template labels to select pods", controller.Namespace, controller.Name)
	}
	return cs.CoreV1().Pods(controller.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(labels).AsSelector().String(),
	})
}

// ensureAgentJob creates the deterministic agent Job; AlreadyExists is a no-op
// (second owner, reconcile re-run).
func ensureAgentJob(ctx context.Context, cs kubernetes.Interface, o InjectOptions, pod *v1.Pod) error {
	_, err := cs.BatchV1().Jobs(o.ManagerNamespace).Create(ctx, genAgentJobSpec(o, pod), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
```

Add to `InjectOptions` nothing further — `Secret` and `Image` already exist. Verify the mesh `envoyRuleSpec` field names match the call above (`Ports` vs `Ports`, `PortMap` vs `PortMap` — docs/17 §5 lists `Ports`/`PortMap`; adjust to the actual struct).

- [ ] **Step 4: Run tests, verify loop**

Run: `go build ./... && go vet ./pkg/... && go test ./pkg/inject/... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/inject/ cmd/kubevpn/cmds/
git commit -m "feat(inject): nodeAgentInjector — rules + node-pinned privileged Job per pod"
```

---

### Task 5: Leave cleanup — delete agent Jobs when the last rule is removed

**Files:**
- Modify: `pkg/inject/mesh.go` (`UnpatchContainer`) and its call sites (`grep -rn "UnpatchContainer(" --include=*.go` — expect `pkg/xds/inject_service.go` and `pkg/handler/`)
- Test: `pkg/inject/nodeagent_leave_test.go`

**Interfaces:**
- Consumes: `UnpatchContainer(ctx, nodeID, factory, configMapInterface, object, ownerID)` — its signature GAINS `clientset kubernetes.Interface` and `managerNamespace string` so it can delete Jobs.
- Produces: `func deleteAgentJobs(ctx, cs, managerNamespace, ownerID) error` — Task 7's reconciler also deletes via labels.

- [ ] **Step 1: Write the failing test**

Create `pkg/inject/nodeagent_leave_test.go`:

```go
package inject

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUnpatchContainerDeletesAgentJobsOnEmpty(t *testing.T) {
	cs, opts, pod := newFakeWorld(t) // same helper as Task 4
	_ = NewInjector(opts).Inject(context.Background())
	// owner-1 leaves: remove its rule → job must be deleted
	if err := UnpatchContainer(context.Background(), opts.NodeID, opts.Factory,
		cs.CoreV1().ConfigMaps(opts.ManagerNamespace), opts.Controller, opts.OwnerID,
		cs, opts.ManagerNamespace); err != nil {
		t.Fatalf("unpatch: %v", err)
	}
	jobs, _ := cs.BatchV1().Jobs(opts.ManagerNamespace).List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Fatalf("agent job must be deleted when the last rule is removed, got %d", len(jobs.Items))
	}
}

func TestUnpatchContainerKeepsJobsWhileOtherOwnersRemain(t *testing.T) {
	cs, opts, pod := newFakeWorld(t)
	_ = NewInjector(opts).Inject(context.Background())
	opts2 := opts
	opts2.OwnerID = "owner-2"
	opts2.Headers = map[string]string{"X-User": "b"}
	_ = NewInjector(opts2).Inject(context.Background())
	_ = UnpatchContainer(context.Background(), opts.NodeID, opts.Factory,
		cs.CoreV1().ConfigMaps(opts.ManagerNamespace), opts.Controller, opts.OwnerID,
		cs, opts.ManagerNamespace)
	jobs, _ := cs.BatchV1().Jobs(opts.ManagerNamespace).List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 {
		t.Fatalf("job must survive while owner-2 still holds rules, got %d", len(jobs.Items))
	}
}
```

Note the workload was never patched in this mode, so the existing RemoveContainers/patch path must be a no-op here — the test implicitly verifies no error and no patch. If the existing `UnpatchContainer` unconditionally patches the controller even when no kubevpn containers exist, guard it: skip the patch when `templateSpec` carries no kubevpn containers.

- [ ] **Step 2: Run to verify it fails**, then implement.

- [ ] **Step 3: Implement**

In `pkg/inject/mesh.go`, extend `UnpatchContainer` with the two new parameters, and after the "empty" branch (the one that currently calls RemoveContainers + patch), add:

```go
// node-hosted agent Jobs are deleted when this owner's last rule is removed
// (docs/55 §6). For sidecar-mode workloads there are no such Jobs and this is
// a no-op; the workload patch above is likewise a no-op in node-agent mode.
if err := deleteAgentJobs(ctx, clientset, managerNamespace, ownerID); err != nil {
	plog.G(ctx).Warnf("delete agent jobs for owner %s: %v", ownerID, err)
}
```

Create the helper (same file or `pkg/inject/nodeagent_job.go`):

```go
// deleteAgentJobs removes every agent Job owned by ownerID. Best-effort by
// callers; the manager lease reaper is the backstop (docs/55 §6).
func deleteAgentJobs(ctx context.Context, cs kubernetes.Interface, managerNamespace, ownerID string) error {
	jobs, err := cs.BatchV1().Jobs(managerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"kubevpn/owner-id": ownerID}}),
	})
	if err != nil {
		return fmt.Errorf("list agent jobs: %w", err)
	}
	for i := range jobs.Items {
		if err := cs.BatchV1().Jobs(managerNamespace).Delete(ctx, jobs.Items[i].Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete agent job %s: %w", jobs.Items[i].Name, err)
		}
	}
	return nil
}
```

Update every `UnpatchContainer(` call site (grep listed in Files) to pass `clientset, managerNamespace`. In `pkg/xds/inject_service.go` both values come from `s.clientset` / `s.namespace`.

- [ ] **Step 4: Run tests, verify loop, commit**

Run: `go build ./... && go vet ./pkg/... && go test ./pkg/inject/... ./pkg/xds/... ./pkg/handler/...`
Expected: PASS.

```bash
git add pkg/inject/ pkg/xds/ pkg/handler/
git commit -m "feat(inject): leave deletes node-hosted agent Jobs by owner label"
```

---

### Task 6: Attach-mode configuration (code + Helm, kept in sync)

**Files:**
- Modify: `pkg/cluster/manager/resources.go` (manager Role rules + `genDeploySpec` env)
- Modify: `charts/kubevpn/values.yaml`, `charts/kubevpn/templates/deployment.yaml`, `charts/kubevpn/templates/role.yaml`
- Test: `pkg/cluster/manager/resources_attach_test.go`

**Interfaces:**
- Consumes: `config.AttachMode*` constants (Task 1).
- Produces: env var `KUBEVPN_PROXY_ATTACH_MODE` on the traffic-manager Deployment; `proxy.attachMode` Helm value; `batch/jobs` rules on the manager-namespace Role.

- [ ] **Step 1: Write the failing test**

Create `pkg/cluster/manager/resources_attach_test.go`:

```go
package manager

import (
	"strings"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestGenDeploySpecCarriesAttachModeEnv(t *testing.T) {
	deploy := genDeploySpec("kubevpn", config.Image, "")
	found := false
	for _, c := range deploy.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "KUBEVPN_PROXY_ATTACH_MODE" && e.Value == config.AttachModeSidecar {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("traffic-manager deploy must carry KUBEVPN_PROXY_ATTACH_MODE (default sidecar)")
	}
}

func TestManagerRoleGrantsJobs(t *testing.T) {
	role := genManagerRole("kubevpn") // the namespaced Role generator used by createOutboundPod — verify exact name with `grep -n "func gen" pkg/cluster/manager/resources.go`
	grants := false
	for _, r := range role.Rules {
		for _, g := range r.APIGroups {
			if g != "batch" {
				continue
			}
			for _, res := range r.Resources {
				if res == "jobs" {
					grants = true
				}
			}
		}
	}
	if !grants {
		t.Fatal("manager namespace Role must grant batch/jobs create/delete for node-agent Jobs")
	}
}
```

- [ ] **Step 2: Run to verify it fails**, then implement:

1. `genDeploySpec`: add `{Name: "KUBEVPN_PROXY_ATTACH_MODE", Value: attachMode}` where `attachMode` is a new parameter (thread it from `createOutboundPod`'s Helm-value/env source; the code path default is `config.AttachModeSidecar`).
2. The namespaced manager Role generator: add rule `APIGroups: ["batch"], Resources: ["jobs"], Verbs: ["create", "get", "list", "watch", "delete"]`.
3. `charts/kubevpn/values.yaml`: add

```yaml
proxy:
  attachMode: sidecar
```

4. `charts/kubevpn/templates/deployment.yaml`: in the container `env` list add

```yaml
- name: KUBEVPN_PROXY_ATTACH_MODE
  value: {{ .Values.proxy.attachMode | default "sidecar" }}
```

5. `charts/kubevpn/templates/role.yaml` (the manager-namespace Role): add the same batch/jobs rule as (2). The ClusterRole template needs no change (wildcard — measured).

- [ ] **Step 3: Run tests, verify loop, commit**

Run: `go build ./... && go test ./pkg/cluster/... -v && helm lint charts/kubevpn` (if helm is installed; otherwise `kubectl create --dry-run=client -f <rendered>` is acceptable — note which was used).
Expected: PASS.

```bash
git add pkg/cluster/manager/ charts/kubevpn/
git commit -m "feat: cluster-level proxy.attachMode config (code + helm, in sync)"
```

---

### Task 7: Reconciler — keep Jobs aligned with pod rescheduling (docs/55 P3)

**Files:**
- Modify: `pkg/controlplane/virtual.go` (add `Selector` field to `Virtual`, yaml tag `selector`)
- Create: `pkg/xds/agent_reconciler.go`
- Modify: `pkg/inject/nodeagent_job.go` (write `Selector` into the rule at inject time via `envoyRuleSpec`)
- Modify: the xds server startup site (`grep -rn "WatchNamespaceRoutes\|leases.Manager" pkg/xds/*.go | head` — start the reconciler next to the existing watchers)
- Test: `pkg/xds/agent_reconciler_test.go`

**Interfaces:**
- Consumes: `ensureAgentJob`, `deleteAgentJobs`, `agentJobName`, `listControllerPods`.
- Produces: `type AgentReconciler struct{...}` + `func (r *AgentReconciler) Start(ctx context.Context) error` (docs/55 §6: reschedule → new Job on the new node; pod deleted → orphan Job deleted; manager restart → Jobs restored from rules).

- [ ] **Step 1: Write the failing test (docs/55 §10 scenarios 3 and 6)**

Create `pkg/xds/agent_reconciler_test.go`:

```go
package xds

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// seedWorld builds: ConfigMap with one Virtual carrying Selector "app=web",
// pod web-1 on node-a with matching labels, agent Job for its UID.
// Copy fixture patterns from existing pkg/xds tests (grep `fake.NewSimpleClientset` pkg/xds/).
func TestReconcilerRecreatesJobAfterPodReschedule(t *testing.T) {
	cs, rec := seedWorld(t) // pod on node-a, job exists
	// reschedule: pod now on node-b
	pod, _ := cs.CoreV1().Pods("ns").Get(context.Background(), "web-1", metav1.GetOptions{})
	pod.Spec.NodeName = "node-b"
	_, _ = cs.CoreV1().Pods("ns").Update(context.Background(), pod, metav1.UpdateOptions{})

	if err := rec.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	job, err := cs.BatchV1().Jobs("kubevpn").Get(context.Background(), agentJobName(pod.UID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job missing after reschedule: %v", err)
	}
	if job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != "node-b" {
		t.Fatalf("job not moved to node-b: %+v", job.Spec.Template.Spec.NodeSelector)
	}
}

func TestReconcilerDeletesOrphanJobWhenPodGone(t *testing.T) {
	cs, rec := seedWorld(t)
	_ = cs.CoreV1().Pods("ns").Delete(context.Background(), "web-1", metav1.DeleteOptions{})
	if err := rec.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	jobs, _ := cs.BatchV1().Jobs("kubevpn").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Fatalf("orphan job must be deleted, got %d", len(jobs.Items))
	}
}

func TestReconcilerRestoresMissingJobAfterManagerRestart(t *testing.T) {
	cs, rec := seedWorld(t)
	_ = cs.BatchV1().Jobs("kubevpn").DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})
	if err := rec.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	jobs, _ := cs.BatchV1().Jobs("kubevpn").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 {
		t.Fatalf("job must be restored from rules, got %d", len(jobs.Items))
	}
}
```

- [ ] **Step 2: Run to verify it fails**, then implement:

1. `pkg/controlplane/virtual.go`: add to `Virtual`:

```go
	// Selector is the workload's pod label selector, recorded at node-agent
	// inject time so the reconciler can match pods to rules (docs/55 §6).
	// Zero value = rule was written by sidecar mode; reconciler skips it.
	Selector string `yaml:"selector,omitempty" json:"selector,omitempty"`
```

2. `pkg/inject`: `envoyRuleSpec` gains `Selector string`; `nodeAgentInjector.Inject` sets it to `labels.Set(templateLabels).AsSelector().String()`; `addEnvoyConfig`/`addVirtualRule` propagate it into the Virtual (verify the rule-merge cases preserve the field on update).

3. Create `pkg/xds/agent_reconciler.go`:

```go
// AgentReconciler keeps node-hosted agent Jobs aligned with the pods their
// rules select (docs/55 §6): reschedule → re-pin; pod gone → delete; manager
// restart → restore. Cross-namespace ownerRefs are forbidden, so Jobs are
// matched by label, not ownerRef.
type AgentReconciler struct {
	clientset        kubernetes.Interface
	managerNamespace string
	virtuals         func(ctx context.Context) ([]*controlplane.Virtual, error) // reads ENVOY_CONFIG via the existing store/informer
	interval         time.Duration
}

func (r *AgentReconciler) Start(ctx context.Context) error {
	interval := r.interval
	if interval == 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := r.reconcileOnce(ctx); err != nil {
				plog.G(ctx).Warnf("agent reconcile: %v", err)
			}
		}
	}
}

func (r *AgentReconciler) reconcileOnce(ctx context.Context) error {
	virtuals, err := r.virtuals(ctx)
	if err != nil {
		return fmt.Errorf("read virtuals: %w", err)
	}
	for _, v := range virtuals {
		if v.Selector == "" {
			continue // sidecar-mode rule
		}
		pods, err := r.clientset.CoreV1().Pods(v.Namespace).List(ctx, metav1.ListOptions{LabelSelector: v.Selector})
		if err != nil {
			return fmt.Errorf("list pods for %s: %w", v.NodeID, err)
		}
		want := map[types.UID]*v1.Pod{}
		for i := range pods.Items {
			want[pods.Items[i].UID] = &pods.Items[i]
		}
		jobs, err := r.clientset.BatchV1().Jobs(r.managerNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "kubevpn/owner-id", // presence: filter precisely per rule below
		})
		if err != nil {
			return fmt.Errorf("list agent jobs: %w", err)
		}
		// delete Jobs whose pod is gone or moved (deterministic name = pod UID)
		for i := range jobs.Items {
			job := &jobs.Items[i]
			uid := types.UID(job.Labels["kubevpn/pod-uid"])
			if uid == "" {
				continue
			}
			if pod, ok := want[uid]; !ok {
				_ = r.clientset.BatchV1().Jobs(r.managerNamespace).Delete(ctx, job.Name, metav1.DeleteOptions{})
			} else if job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != pod.Spec.NodeName {
				_ = r.clientset.BatchV1().Jobs(r.managerNamespace).Delete(ctx, job.Name, metav1.DeleteOptions{})
			}
		}
		// ensure a Job for every live pod (create-or-skip)
		for uid, pod := range want {
			_ = uid
			if err := ensureAgentJob(ctx, r.clientset, InjectOptions{ManagerNamespace: r.managerNamespace, Image: config.Image, OwnerID: v.OwnerID}, pod); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("ensure agent job %s/%s: %w", pod.Namespace, pod.Name, err)
			}
		}
	}
	return nil
}
```

This sketch needs the Job generator's full `InjectOptions` (Secret/Image/NodeID/Ports…). Two corrections to apply when writing the real code: (a) the reconciler must build `genAgentJobSpec` inputs from the Virtual's stored rule, so `genAgentJobSpec` is refactored to take the rule-derived struct rather than `InjectOptions` alone — extract a `type agentJobInput struct { ManagerNamespace, Image, OwnerID string; Secret *v1.Secret; Pod *v1.Pod }` in Task 4 and use it here; (b) the Jobs label selector must scope to this rule's pods (`kubevpn/pod-uid` in the live set is handled by the delete loop, so the broad selector above is correct as written — keep it). Wire `Start` next to the existing xds watchers with `r.virtuals` bound to the ConfigMap store already held by the server.

- [ ] **Step 3: Run tests, verify loop, commit**

Run: `go build ./... && go vet ./pkg/... && go test ./pkg/xds/... ./pkg/controlplane/... ./pkg/inject/...`
Expected: PASS.

```bash
git add pkg/controlplane/ pkg/inject/ pkg/xds/
git commit -m "feat(xds): agent reconciler — reschedule/orphan/restart alignment"
```

---

### Task 8: Docs sync, real-cluster validation, full loop

**Files:**
- Modify: `docs/55-node-hosted-agent.md` (status → implemented; record the `/proc`-scan prototype result from §12)
- Modify: `docs/README.md` (move 55 out of "Planned Directions")
- Test: `pkg/inject/nodeagent_e2e_integration_test.go` — **`//go:build integration`** file, needs the real cluster

**Interfaces:**
- Consumes: everything shipped in Tasks 1–7.
- Produces: validated P1 assumption (docs/55 §12: `/proc` cgroup scan finds the pod netns) and the merged state.

- [ ] **Step 1: Write the integration-tagged e2e test**

Create `pkg/inject/nodeagent_e2e_integration_test.go` with `//go:build integration`:

```go
//go:build integration

package inject

// TestIntegration_NodeAgentJobAttachesNetns validates docs/55 §12's load-bearing
// assumption on a real cluster: the agent Job finds the pod's netns via /proc,
// enters it, and the capture listener answers. Requires a reachable cluster
// (make ut path). Uses the kubevpn image already present in the cluster.
```

The test body follows the repo's existing integration fixtures (`grep -rln "go:build integration" pkg/ | head`): create a namespace, a 1-replica nginx Deployment, run `NewInjector` in node-agent mode against the real clientset, wait for the Job to complete its first container start, then `kubectl exec`-style check that the pod's netns now has the PREROUTING DNAT rule (`iptables -t nat -S PREROUTING` via an exec into the target pod) and contains `15006`. Cleanup deletes the namespace (Job dies with it).

- [ ] **Step 2: Run the full no-cluster loop**

Run: `go build ./... && go vet ./pkg/... && go test ./pkg/... && make ut-compile`
Expected: all PASS.

- [ ] **Step 3: Run the full suite with the test cluster**

Switch to the remote context first (`kubernetes-admin-cf2096c3e67fb41a0b0cfc3de9a72f027` — the default `orbstack` context is unreachable):

```bash
KUBECONFIG=/data/.kube/config make ut
```
Expected: PASS; if the e2e probe above reveals the `/proc` scan misses on the cluster's cgroup driver, record the finding in docs/55 §12 and fall back to the CRI-socket path (docs/55 §4 fallback) as a follow-up commit.

- [ ] **Step 4: Docs sync + commit**

Flip docs/55 status line to `**implemented** (P1-P3)`, move it out of docs/README.md "Planned Directions", and record any §12 assumption outcomes. Commit:

```bash
git add docs/55-node-hosted-agent.md docs/README.md pkg/inject/
git commit -m "docs: node-hosted agent implemented; record /proc-scan validation results"
```

---

## Self-Review Notes (applied while writing)

- Spec coverage: docs/55 §2 R1 (no patch — `nodeAgentInjector.Inject` never calls `patchWorkload`; Task 4), R2 (identical rules — Task 4 reuses `addEnvoyConfig`), R3 (env + Helm, no CLI flag — Task 1 Step 4 + Task 6), R4 (fargate precedence — Task 1 test 3), R5 (delete on empty — Task 5; reaper backstop noted). §6 lifecycle table → Task 5 + Task 7. §7 RBAC → Task 6. §10 scenarios 1,2,5 → Task 4; 4 → Task 5; 3,6 → Task 7; real-cluster → Task 8.
- Type consistency: `agentJobInput` refactor flagged in Task 7 so the reconciler can build Job specs without an `InjectOptions` fabric; `deleteAgentJobs`/`ensureAgentJob`/`agentJobName` names used identically in Tasks 4, 5, 7.
- Known mechanical adjustments allowed at execution: exact names of the fixture builders, the envoy bootstrap render helper, and the manager-Role generator (each has a grep step + expected outcome).
