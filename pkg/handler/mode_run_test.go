package handler

import (
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

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (u *ut) deleteDeployForSaveResource(t *testing.T) {
	for _, s := range []string{"deploy/productpage", "deploy/ratings", "deploy/details"} {
		cmd := exec.Command("kubectl", "delete", s, "--force")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err != nil {
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
	done := make(chan any)
	var once = &sync.Once{}
	go func() {
		stdout, stderr, err := util.RunWithRollingOutWithChecker(cmd, func(log string) (stop bool) {
			contains := strings.Contains(log, "Start listening http port 9080 ...")
			if contains {
				once.Do(func() {
					close(done)
				})
			}
			return contains
		})
		if err != nil {
			select {
			case <-done:
				t.Log(err, stdout, stderr)
			default:
				t.Fatal(err, stdout, stderr)
			}
		}
	}()
	<-done

	app := "authors"
	ip, err := u.getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, localPort)
	u.healthChecker(t, endpoint, nil, remoteSyncPod)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, remoteSyncPod)

	endpoint = fmt.Sprintf("http://%s:%v/health", ip, 9080)
	u.healthChecker(t, endpoint, nil, local)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, local)

	t.Run("kubevpnRunWithFullProxyStatus", u.checkRunWithFullProxyStatus)

	err = cmd.Process.Kill()
	if err != nil {
		t.Fatal(err)
	}
	for cmd.ProcessState == nil {
	}
}

func (u *ut) kubevpnRunWithServiceMesh(t *testing.T) {
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
	done := make(chan any)
	var once = &sync.Once{}
	go func() {
		stdout, stderr, err := util.RunWithRollingOutWithChecker(cmd, func(log string) (stop bool) {
			contains := strings.Contains(log, "Start listening http port 9080 ...")
			if contains {
				once.Do(func() {
					close(done)
				})
			}
			return contains
		})
		if err != nil {
			select {
			case <-done:
				t.Log(err, stdout, stderr)
			default:
				t.Fatal(err, stdout, stderr)
			}
		}
	}()
	<-done

	app := "authors"
	ip, err := u.getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, localPort)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, remoteSyncPod)

	endpoint = fmt.Sprintf("http://%s:%v/health", ip, 9080)
	u.healthChecker(t, endpoint, nil, remoteSyncOrigin)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, local)

	t.Run("kubevpnRunWithServiceMeshStatus", u.checkRunWithServiceMeshStatus)

	err = cmd.Process.Kill()
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

	expect := status{List: []*connection{{
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

	var statuses status
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

	expect := status{List: []*connection{{
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

	var statuses status
	if err = json.Unmarshal(output, &statuses); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(statuses, expect) {
		marshal, _ := json.Marshal(expect)
		marshalB, _ := json.Marshal(statuses)
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(marshalB))
	}
}
