//go:build integration

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func (u *sshUt) deleteDeployForSaveResource(t *testing.T) {
	options := metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}
	for _, s := range []string{"productpage", "ratings"} {
		err := u.clientset.AppsV1().Deployments(u.namespace).Delete(context.Background(), s, options)
		if err != nil && !errors.IsNotFound(err) {
			t.Fatal(err)
		}
	}
}

func (u *sshUt) resetDeployAuthors(t *testing.T) {
	cmd := exec.Command("kubevpn", "reset", "deploy/authors")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatalf("error resetting deploy/authors: %v", err)
	}
}

func (u *sshUt) kubevpnRunWithFullProxy(t *testing.T) {
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

func (u *sshUt) kubevpnRunWithServiceMesh(t *testing.T) {
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

func (u *sshUt) checkRunWithFullProxyStatus(t *testing.T) {
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

func (u *sshUt) checkRunWithServiceMeshStatus(t *testing.T) {
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
