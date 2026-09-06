//go:build integration

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// runStartupTimeout bounds how long a `kubevpn run` may take to log its startup marker.
// Without it, a run whose process stays alive but never starts (e.g. a crash-looping
// injected sidecar) hangs the test until the global go-test timeout.
const runStartupTimeout = 6 * time.Minute

// waitRunStartup runs cmd (a `kubevpn run ...`) and blocks until it logs marker, fails
// fast if it exits before startup, or — on runStartupTimeout — kills the process (via
// cancel) and dumps the KubeVPN sidecar logs before failing. Shared by the SSH and
// non-SSH run tests; the caller keeps cmd to signal/stop it once startup succeeds.
func waitRunStartup(t *testing.T, cmd *exec.Cmd, cancel context.CancelFunc, clientset kubernetes.Interface, marker string) {
	t.Helper()
	done := make(chan any)
	startupErr := make(chan error, 1)
	var once sync.Once
	// rolling captures every line the run container emits, tee'd from the checker as it
	// streams. RunWithRollingOutWithChecker only returns its own buffer after the process
	// exits, so when a hung run is killed but does NOT exit (the case below) its output
	// is otherwise lost — we keep our own copy here to dump on timeout.
	var rollingMu sync.Mutex
	var rolling bytes.Buffer
	go func() {
		stdout, stderr, err := util.RunWithRollingOutWithChecker(cmd, func(log string) (stop bool) {
			rollingMu.Lock()
			rolling.WriteString(log + "\n")
			rollingMu.Unlock()
			if strings.Contains(log, marker) {
				once.Do(func() { close(done) })
				return true
			}
			return false
		})
		if err != nil {
			select {
			case <-done:
				t.Log(err, stdout, stderr)
			default:
				// Exited before startup completed. Signal the waiter instead of calling
				// t.Fatal from this non-test goroutine (which would not stop the test and
				// would leave the waiter blocked forever).
				startupErr <- fmt.Errorf("kubevpn run exited before startup: %w, stdout: %s, stderr: %s", err, stdout, stderr)
			}
		}
	}()
	select {
	case <-done:
	case err := <-startupErr:
		t.Fatal(err)
	case <-time.After(runStartupTimeout):
		cancel() // kill the run process so the reader goroutine returns its captured output
		rollingMu.Lock()
		captured := rolling.String()
		rollingMu.Unlock()
		// Dump the run container's own output FIRST — it is the local kubevpn that failed
		// to start and is the primary diagnostic; the sidecar logs are secondary context.
		t.Logf("===== kubevpn run container output captured before timeout =====\n%s", captured)
		dumpKubeVPNSidecarLogs(t, clientset)
		select {
		case err := <-startupErr:
			t.Fatalf("kubevpn run did not log %q within %s — an injected sidecar is likely crash-looping "+
				"(run container output and sidecar logs dumped above): %v", marker, runStartupTimeout, err)
		case <-time.After(15 * time.Second):
			t.Fatalf("kubevpn run did not log %q within %s and did not exit after cancel "+
				"(run container output and sidecar logs dumped above)", marker, runStartupTimeout)
		}
	}
}

// forceCleanupRunContainers removes any `kubevpn run` containers left behind for the given
// app label. Run mode starts detached docker containers (publishing the workload's ports);
// when a run test fails, the deferred context cancel SIGKILLs `kubevpn run` before it can
// stop them, so the containers keep their published ports (e.g. :80) and the NEXT run test
// fails with "port is already allocated" and times out. Registered via t.Cleanup so one
// failure does not cascade. Best-effort: only logs on error.
func forceCleanupRunContainers(t *testing.T, app string) {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=app="+app).CombinedOutput()
	if err != nil {
		t.Logf("list leftover run containers (app=%s): %v: %s", app, err, out)
		return
	}
	for _, id := range strings.Fields(string(out)) {
		if rmOut, rmErr := exec.Command("docker", "rm", "-f", id).CombinedOutput(); rmErr != nil {
			t.Logf("force-remove leftover run container %s: %v: %s", id, rmErr, rmOut)
		} else {
			t.Logf("force-removed leftover run container %s (app=%s)", id, app)
		}
	}
}

func (u *ut) deleteDeployForSaveResource(t *testing.T) {
	options := metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}
	for _, s := range []string{"productpage", "ratings"} {
		err := u.clientset.AppsV1().Deployments(u.namespace).Delete(context.Background(), s, options)
		if err != nil && !errors.IsNotFound(err) {
			t.Fatal(err)
		}
	}
}

func (u *ut) resetDeployAuthors(t *testing.T) {
	cmd := exec.Command("kubevpn", "reset", "deploy/authors")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatalf("error resetting deploy/authors: %v", err)
	}
}

func (u *ut) kubevpnRunWithFullProxy(t *testing.T) {
	t.Cleanup(func() { forceCleanupRunContainers(t, "authors") })
	path := u.writeTempFile(t)
	name := filepath.Base(path)
	dir := filepath.Dir(path)
	remoteDir := "/app/test"
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	localPort := 9090

	cmd := exec.CommandContext(ctx, "kubevpn",
		"run", "deploy/authors",
		"-c", "authors",
		"--debug",
		"-v", fmt.Sprintf("%s:%s", dir, remoteDir),
		"-p", fmt.Sprintf("%d:9080", localPort),
		"--tty=false", //https://github.com/actions/runner/issues/241
		"--rm",
		"--entrypoint", "go", "run", fmt.Sprintf("%s/%s", remoteDir, name),
	)
	waitRunStartup(t, cmd, cancelFunc, u.clientset, "Start listening http port 9080 ...")

	app := "authors"
	ip, err := u.getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}

	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	u.healthChecker(t, endpoint, nil, local)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, local)

	t.Run("kubevpnRunWithFullProxyStatus", u.checkRunWithFullProxyStatus)
	t.Run("commonTest", u.commonTest)

	err = cmd.Process.Signal(os.Interrupt)
	if err != nil {
		t.Fatal(err)
	}
	for cmd.ProcessState == nil {
	}
}

func (u *ut) kubevpnRunWithServiceMesh(t *testing.T) {
	t.Cleanup(func() { forceCleanupRunContainers(t, "authors") })
	path := u.writeTempFile(t)
	name := filepath.Base(path)
	dir := filepath.Dir(path)
	remoteDir := "/app/test"

	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	localPort := 9090

	cmd := exec.CommandContext(ctx, "kubevpn",
		"run", "deploy/authors",
		"-c", "authors",
		"--debug",
		"--headers", "env=test",
		"-v", fmt.Sprintf("%s:%s", dir, remoteDir),
		"-p", fmt.Sprintf("%d:9080", localPort),
		"--tty=false", //https://github.com/actions/runner/issues/241
		"--rm",
		"--entrypoint", "go", "run", fmt.Sprintf("%s/%s", remoteDir, name),
	)
	waitRunStartup(t, cmd, cancelFunc, u.clientset, "Start listening http port 9080 ...")

	app := "authors"
	ip, err := u.getServiceIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", "localhost", localPort)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, remoteSyncPod)
	u.healthChecker(t, endpoint, nil, remoteSyncPod)

	u.waitManagerReady(t)
	endpoint = fmt.Sprintf("http://%s:%v/health", ip, 9080)
	u.meshHealthChecker(t, endpoint, nil, remoteSyncOrigin)
	u.meshHealthChecker(t, endpoint, map[string]string{"env": "test"}, local)

	t.Run("kubevpnRunWithServiceMeshStatus", u.checkRunWithServiceMeshStatus)
	t.Run("commonTest", u.commonTest)

	err = cmd.Process.Signal(os.Interrupt)
	if err != nil {
		t.Fatal(err)
	}
	for cmd.ProcessState == nil {
	}
}

func (u *ut) checkRunWithFullProxyStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(output))
	}

	expect := connStatus{List: []*connection{{
		Namespace: u.namespace,
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: u.namespace,
			Workload:  "deployments.apps/authors",
			RuleList: []*proxyRule{{
				Headers:       nil,
				CurrentDevice: true,
				PortMap:       map[int32]int32{9080: 9080, 80: 80},
			}},
		}},
	}}}

	var statuses connStatus
	if err = json.Unmarshal(output, &statuses); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(statuses, expect) {
		marshal, _ := json.Marshal(expect)
		marshalB, _ := json.Marshal(statuses)
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(marshalB))
	}
}

func (u *ut) checkRunWithServiceMeshStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(output))
	}

	expect := connStatus{List: []*connection{{
		Namespace: u.namespace,
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: u.namespace,
			Workload:  "deployments.apps/authors",
			RuleList: []*proxyRule{{
				Headers:       map[string]string{"env": "test"},
				CurrentDevice: true,
				PortMap:       map[int32]int32{9080: 9080, 80: 80},
			}},
		}},
	}}}

	var statuses connStatus
	if err = json.Unmarshal(output, &statuses); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(statuses, expect) {
		marshal, _ := json.Marshal(expect)
		marshalB, _ := json.Marshal(statuses)
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(marshalB))
	}
}
