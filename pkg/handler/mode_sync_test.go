//go:build integration

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// syncCmdTimeout bounds a `kubevpn sync` invocation. The cloned sync pod carries an
// injected vpn sidecar that runs `kubevpn proxy --foreground` (see genVPNContainer);
// if that sidecar exits and goes CrashLoopBackOff, `kubevpn sync` waits forever for
// the pod to become ready. Without a bound the test hangs until the global `go test`
// timeout (observed: a 2h CI stall). This caps it so the test fails in minutes with
// the pod-status output the command already streams.
const syncCmdTimeout = 6 * time.Minute

// runSyncCommand runs `kubevpn sync <args>` under a bounded context, streaming output.
// On failure (timeout or non-zero exit) it dumps the injected KubeVPN sidecar logs — the
// usual root cause is a sidecar crash-looping, and the test otherwise only shows pod
// status, never the container logs needed to diagnose it. Shared by the SSH and non-SSH
// sync tests.
func runSyncCommand(t *testing.T, ctx context.Context, clientset kubernetes.Interface, args []string) {
	t.Helper()
	syncCtx, cancel := context.WithTimeout(ctx, syncCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(syncCtx, "kubevpn", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if syncCtx.Err() == context.DeadlineExceeded {
		dumpKubeVPNSidecarLogs(t, clientset)
		t.Fatalf("kubevpn sync did not finish within %s — the injected %q sidecar is likely crash-looping "+
			"(sidecar logs dumped above)", syncCmdTimeout, config.ContainerSidecarVPN)
	}
	if err != nil {
		dumpKubeVPNSidecarLogs(t, clientset)
		t.Fatalf("kubevpn sync failed: %v (sidecar logs dumped above)", err)
	}
}

// kubeVPNSidecars are the container names KubeVPN injects. On an e2e failure their logs
// (not just pod status) are what explain why traffic did not route or a pod crash-looped.
var kubeVPNSidecars = sets.New[string](config.ContainerSidecarVPN, config.ContainerSidecarEnvoyProxy)

// dumpKubeVPNSidecarLogs prints, across all namespaces, the status and container logs
// (previous crashed instance first, then current) of every KubeVPN sidecar (vpn,
// envoy-proxy). The e2e tests otherwise only dump pod status/describe, never the sidecar
// logs that surface *why* `kubevpn proxy --foreground` exited or envoy failed to route.
// Best-effort: it logs and continues past any API error, and uses its own short-lived
// context (a caller's context may already be dead).
func dumpKubeVPNSidecarLogs(t *testing.T, clientset kubernetes.Interface) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pods, err := clientset.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Logf("dump kubevpn sidecar logs: list pods: %v", err)
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		// Container state (restart count + last-termination exit code/reason) survives a
		// crash-loop and pinpoints whether a sidecar OOMed, errored, or exited 0.
		for _, cs := range pod.Status.ContainerStatuses {
			if !kubeVPNSidecars.Has(cs.Name) {
				continue
			}
			t.Logf("sidecar %s/%s [%s]: restartCount=%d state=%+v lastTermination=%+v",
				pod.Namespace, pod.Name, cs.Name, cs.RestartCount, cs.State, cs.LastTerminationState)
		}
		for _, c := range pod.Spec.Containers {
			if !kubeVPNSidecars.Has(c.Name) {
				continue
			}
			for _, previous := range []bool{true, false} {
				which := "current"
				if previous {
					which = "previous"
				}
				req := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
					Container: c.Name,
					Previous:  previous,
				})
				stream, err := req.Stream(ctx)
				if err != nil {
					t.Logf("dump sidecar logs (%s) %s/%s [%s]: %v", which, pod.Namespace, pod.Name, c.Name, err)
					continue
				}
				data, _ := io.ReadAll(stream)
				_ = stream.Close()
				if len(data) > 0 {
					t.Logf("===== sidecar %s logs (%s instance) %s/%s =====\n%s",
						c.Name, which, pod.Namespace, pod.Name, string(data))
				}
			}
		}
	}
}

var content = `package main

import (
	"encoding/json"
	"log"
	"net/http"
)

func main() {
	http.HandleFunc("/health", health)

	log.Println("Start listening http port 9080 ...")
	if err := http.ListenAndServe(":9080", nil); err != nil {
		panic(err)
	}
}

func health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	resp, err := json.Marshal(map[string]string{
		"status": "Authors is healthy in pod",
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Println(err)
		return
	}
	w.Write(resp)
}`

func (u *ut) kubevpnSyncWithFullProxy(t *testing.T) {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	endpoint := u.kubevpnSync(t, ctx, false)
	u.healthChecker(t, endpoint, nil, remoteSyncPod)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, remoteSyncPod)
}

func (u *ut) kubevpnSync(t *testing.T, ctx context.Context, isServiceMesh bool) string {
	path := u.writeTempFile(t)
	name := filepath.Base(path)
	dir := filepath.Dir(path)
	remoteDir := "/app/test"

	args := []string{"sync",
		"deploy/authors",
		"--debug",
		"--sync", fmt.Sprintf("%s:%s", dir, remoteDir),
		"-c", "authors",
		"--target-image", "ghcr.io/kubenetworks/authors:latest",
	}
	if isServiceMesh {
		args = append(args, "--headers", "env=test")
	}
	runSyncCommand(t, ctx, u.clientset, args)

	list, err := util.GetRunningPodList(ctx, u.clientset, u.namespace, fields.OneTermEqualSelector("origin-workload", "authors").String())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatal("expect at least one pod")
	}

	remotePath := fmt.Sprintf("%s/%s", remoteDir, name)
	containerName := "authors"
	u.checkContent(ctx, t, list[0].Name, containerName, remotePath)
	go u.execServer(ctx, t, list[0].Name, containerName, remotePath)

	endpoint := fmt.Sprintf("http://%s:%v/health", list[0].Status.PodIP, 9080)
	u.healthChecker(t, endpoint, nil, remoteSyncPod)
	app := "authors"
	ip, err := u.getServiceIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint = fmt.Sprintf("http://%s:%v/health", ip, 9080)
	return endpoint
}

func (u *ut) execServer(ctx context.Context, t *testing.T, podName string, containerName string, remoteDir string) {
	for ctx.Err() == nil {
		output, err := util.Shell(
			ctx,
			u.clientset,
			u.restconfig,
			podName,
			containerName,
			u.namespace,
			[]string{"go", "run", remoteDir},
		)
		if err != nil {
			t.Log(err, output)
		}
		time.Sleep(time.Second * 1)
	}
}

func (u *ut) kubevpnSyncWithServiceMesh(t *testing.T) {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	endpoint := u.kubevpnSync(t, ctx, true)
	u.healthChecker(t, endpoint, nil, remoteSyncOrigin)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, remoteSyncPod)
}

func (u *ut) checkContent(ctx context.Context, t *testing.T, podName string, containerName string, remotePath string) {
	err := retry.OnError(
		wait.Backoff{Duration: time.Second, Factor: 1, Jitter: 0, Steps: 120},
		func(err error) bool { return err != nil },
		func() error {
			shell, err := util.Shell(
				ctx,
				u.clientset,
				u.restconfig,
				podName,
				containerName,
				u.namespace,
				[]string{"cat", remotePath},
			)
			if err != nil {
				return err
			}
			if strings.TrimSpace(shell) != strings.TrimSpace(content) {
				t.Logf("expect: %s, but was: %s", content, shell)
				return fmt.Errorf("expect: %s, but was: %s", content, shell)
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCompile(t *testing.T) {
	u := &ut{}
	u.writeTempFile(t)
}

func (u *ut) writeTempFile(t *testing.T) string {
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	temp, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	_, err = temp.WriteString(content)
	if err != nil {
		t.Fatal(err)
	}
	err = temp.Chmod(0755)
	if err != nil {
		t.Fatal(err)
	}
	err = temp.Close()
	if err != nil {
		t.Fatal(err)
	}
	absPath, err := filepath.Abs(temp.Name())
	if err != nil {
		t.Fatal(err)
	}
	return absPath
}

func (u *ut) checkSyncWithFullProxyStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(output))
	}

	expect := status{List: []*connection{{
		Namespace: u.namespace,
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: u.namespace,
			Workload:  "deployments.apps/authors",
			RuleList: []*proxyRule{{
				Headers:       nil,
				CurrentDevice: false,
				PortMap:       map[int32]int32{9080: 9080, 80: 80},
			}},
		}},
		SyncList: []*syncItem{{
			Namespace: u.namespace,
			Workload:  "deploy/authors",
			RuleList:  []*syncRule{{}},
		}},
	}}}

	var statuses status
	if err = json.Unmarshal(output, &statuses); err != nil {
		t.Fatal(err)
	}

	if len(expect.List) == 0 || len(expect.List[0].SyncList) == 0 || len(expect.List[0].SyncList[0].RuleList) == 0 {
		t.Fatal("expect List[0].SyncList[0].RuleList[0] not found", string(output))
	}

	expect.List[0].SyncList[0].RuleList[0].DstWorkload = statuses.List[0].SyncList[0].RuleList[0].DstWorkload

	if !reflect.DeepEqual(statuses, expect) {
		marshal, _ := json.Marshal(expect)
		marshalB, _ := json.Marshal(statuses)
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(marshalB))
	}
}

func (u *ut) kubevpnUnSync(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(output))
	}

	var statuses status
	if err = json.Unmarshal(output, &statuses); err != nil {
		t.Fatal(err)
	}

	if len(statuses.List) == 0 || len(statuses.List[0].SyncList) == 0 || len(statuses.List[0].SyncList[0].RuleList) == 0 {
		t.Fatal("expect List[0].SyncList[0].RuleList[0] not found", string(output))
	}

	cmd = exec.Command("kubevpn", "unsync", statuses.List[0].SyncList[0].RuleList[0].DstWorkload)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func (u *ut) checkSyncWithServiceMeshStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(output))
	}

	expect := status{List: []*connection{{
		Namespace: u.namespace,
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: u.namespace,
			Workload:  "deployments.apps/authors",
			RuleList: []*proxyRule{{
				Headers:       map[string]string{"env": "test"},
				CurrentDevice: false,
				PortMap:       map[int32]int32{9080: 9080, 80: 80},
			}},
		}},
		SyncList: []*syncItem{{
			Namespace: u.namespace,
			Workload:  "deploy/authors",
			RuleList:  []*syncRule{{}},
		}},
	}}}

	var statuses status
	if err = json.Unmarshal(output, &statuses); err != nil {
		t.Fatal(err)
	}

	if len(expect.List) == 0 || len(expect.List[0].SyncList) == 0 || len(expect.List[0].SyncList[0].RuleList) == 0 {
		t.Fatal("expect List[0].SyncList[0].RuleList[0] not found", string(output))
	}

	expect.List[0].SyncList[0].RuleList[0].DstWorkload = statuses.List[0].SyncList[0].RuleList[0].DstWorkload

	if !reflect.DeepEqual(statuses, expect) {
		marshal, _ := json.Marshal(expect)
		marshalB, _ := json.Marshal(statuses)
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(marshalB))
	}
}
