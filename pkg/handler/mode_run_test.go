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
	"syscall"
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
//
// It returns a channel closed once the run process has exited AND its final output has
// been logged; stopRunCmd waits on it to tear the run down without busy-spinning on
// cmd.ProcessState (a data race against os/exec's Wait). The two t.Fatal branches below
// end the test and never return, so only the successful-startup path yields the channel.
func waitRunStartup(t *testing.T, cmd *exec.Cmd, cancel context.CancelFunc, clientset kubernetes.Interface, marker string) <-chan struct{} {
	t.Helper()
	done := make(chan any)
	startupErr := make(chan error, 1)
	// exited is closed by the reader goroutine after the run process exits and its output
	// has been logged, so a waiter that unblocks on it is guaranteed to see that output.
	exited := make(chan struct{})
	var once sync.Once
	// rolling captures every line the run container emits, tee'd from the checker as it
	// streams. RunWithRollingOutWithChecker only returns its own buffer after the process
	// exits, so when a hung run is killed but does NOT exit (the case below) its output
	// is otherwise lost — we keep our own copy here to dump on timeout.
	var rollingMu sync.Mutex
	var rolling bytes.Buffer
	go func() {
		defer close(exited)
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
				// Startup already succeeded; the process has now exited (normally on stop,
				// or after stopRunCmd's SIGQUIT dumps its goroutine stacks). Always log the
				// captured output so that diagnostic dump lands in the test log.
				t.Logf("===== kubevpn run exited: %v =====\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
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
		return exited
	case err := <-startupErr:
		t.Fatal(err)
		return exited
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
	// Unreachable: every case above either returns or calls t.Fatal(f) (which ends the
	// test via runtime.Goexit). Present so the function satisfies its return type.
	return exited
}

// runStopTimeout bounds how long stopRunCmd waits for a `kubevpn run` to exit after
// SIGINT before forcing it down. It must exceed the client-side teardown budget in
// pkg/run/connect.go (leaveTeardownTimeout + disconnectTeardownTimeout ≈ 5m) so a
// healthy, still-progressing teardown completes on its own; only a genuinely wedged
// process reaches the SIGQUIT/SIGKILL path below. vars (not consts) so tests can shrink
// them. runStopQuitGrace is how long we wait after SIGQUIT (for the goroutine dump to
// print and the process to die) before the SIGKILL backstop.
var (
	runStopTimeout   = 6 * time.Minute
	runStopQuitGrace = 30 * time.Second
)

// stopRunCmd stops a started `kubevpn run` and blocks until it exits. It sends SIGINT
// (mirroring Ctrl-C) so the run performs its normal leave+disconnect teardown, then waits
// on exited. If the process is still alive after runStopTimeout the teardown is wedged
// (e.g. against an unreachable cluster): tryStopRun sends SIGQUIT to dump the run/daemon
// goroutine stacks — captured by waitRunStartup and logged — so the blocker can be
// pinpointed, then falls back to cancel() (CommandContext → SIGKILL). Replaces a
// `for cmd.ProcessState == nil {}` busy-wait that had no timeout (turning a transient
// cluster hiccup into a 2h test hang), raced os/exec's Wait on ProcessState, and pinned a
// CPU core spinning. A wedged teardown fails the test (loudly, fast) rather than hanging.
func stopRunCmd(t *testing.T, cmd *exec.Cmd, cancel context.CancelFunc, exited <-chan struct{}) {
	t.Helper()
	killed, err := tryStopRun(cmd, cancel, exited)
	if err != nil {
		t.Fatal(err)
	}
	if killed {
		t.Errorf("kubevpn run did not exit within %s after SIGINT; SIGQUIT/SIGKILL used "+
			"(goroutine dump logged above) — teardown is likely wedged against the cluster", runStopTimeout)
	}
}

// tryStopRun implements the stop sequence for stopRunCmd, free of *testing.T so the
// anti-hang guarantee is unit-testable. It returns killed=true when the process had to be
// force-terminated (SIGQUIT/SIGKILL) rather than exiting cleanly after SIGINT. It always
// returns once the process has exited (exited closed), never hangs.
func tryStopRun(cmd *exec.Cmd, cancel context.CancelFunc, exited <-chan struct{}) (killed bool, err error) {
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		return false, err
	}
	select {
	case <-exited:
		return false, nil
	case <-time.After(runStopTimeout):
	}
	// Wedged: SIGQUIT first so a Go process prints its goroutine stacks (reader captures
	// them), then SIGKILL via cancel() as the ultimate backstop.
	_ = cmd.Process.Signal(syscall.SIGQUIT)
	select {
	case <-exited:
	case <-time.After(runStopQuitGrace):
		cancel() // SIGKILL backstop
		<-exited
	}
	return true, nil
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
	exited := waitRunStartup(t, cmd, cancelFunc, u.clientset, "Start listening http port 9080 ...")

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

	stopRunCmd(t, cmd, cancelFunc, exited)
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
	exited := waitRunStartup(t, cmd, cancelFunc, u.clientset, "Start listening http port 9080 ...")

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

	stopRunCmd(t, cmd, cancelFunc, exited)
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
