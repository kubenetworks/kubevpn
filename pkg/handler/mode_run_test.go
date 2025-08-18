package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func resetDeployAuthors(t *testing.T) {
	cmd := exec.Command("kubevpn", "reset", "deploy/authors")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("error resetting deploy/authors: %s", string(output))
	}
}

func kubevpnRunWithFullProxy(t *testing.T) {
	path := writeTempFile(t)
	name := filepath.Base(path)
	dir := filepath.Dir(path)
	remoteDir := "/app/test"
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	cmd := exec.CommandContext(ctx, "kubevpn",
		"run", "deploy/authors",
		"-c", "authors",
		"--debug",
		"-v", fmt.Sprintf("%s:%s", dir, remoteDir),
		"--entrypoint", fmt.Sprintf("go run %s/%s", remoteDir, name),
	)
	stdout, stderr, err := util.RunWithRollingOutWithChecker(cmd, func(log string) (stop bool) {
		return strings.Contains(log, "Start listening http port 9080 ...")
	})
	if err != nil {
		t.Fatal(err, stdout, stderr)
	}
	app := "authors"
	ip, err := getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	healthChecker(t, endpoint, nil, remoteSyncPod)
	healthChecker(t, endpoint, map[string]string{"env": "test"}, remoteSyncPod)

	t.Run("kubevpnRunWithFullProxyStatus", checkRunWithFullProxyStatus)
}

func kubevpnRunWithServiceMesh(t *testing.T) {
	path := writeTempFile(t)
	name := filepath.Base(path)
	dir := filepath.Dir(path)
	remoteDir := "/app/test"

	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	cmd := exec.CommandContext(ctx, "kubevpn",
		"run", "deploy/authors",
		"-c", "authors",
		"--debug",
		"--headers", "env=test",
		"-v", fmt.Sprintf("%s:%s", dir, remoteDir),
		"--entrypoint", fmt.Sprintf("go run %s/%s", remoteDir, name),
	)
	stdout, stderr, err := util.RunWithRollingOutWithChecker(cmd, func(log string) (stop bool) {
		return strings.Contains(log, "Start listening http port 9080 ...")
	})
	if err != nil {
		t.Fatal(err, stdout, stderr)
	}

	app := "authors"
	ip, err := getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	healthChecker(t, endpoint, nil, remoteSyncOrigin)
	healthChecker(t, endpoint, map[string]string{"env": "test"}, remoteSyncPod)

	t.Run("kubevpnRunWithServiceMeshStatus", checkRunWithServiceMeshStatus)
}

func checkRunWithFullProxyStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(output))
	}

	expect := status{List: []*connection{{
		Namespace: "kubevpn",
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: namespace,
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
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(output))
	}
}

func checkRunWithServiceMeshStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(output))
	}

	expect := status{List: []*connection{{
		Namespace: "kubevpn",
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: namespace,
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
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(output))
	}
}
