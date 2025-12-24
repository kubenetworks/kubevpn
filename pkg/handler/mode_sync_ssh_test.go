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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (u *sshUt) kubevpnSyncWithFullProxy(t *testing.T) {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	endpoint := u.kubevpnSync(t, ctx, false)
	u.healthChecker(t, endpoint, nil, remoteSyncPod)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, remoteSyncPod)
}

func (u *sshUt) kubevpnSync(t *testing.T, ctx context.Context, isServiceMesh bool) string {
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
		"--ssh-addr", "localhost:2222",
		"--ssh-username", "naison",
		"--ssh-password", "naison",
	}
	if isServiceMesh {
		args = append(args, "--headers", "env=test")
	}
	cmd := exec.Command("kubevpn", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

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

func (u *sshUt) execServer(ctx context.Context, t *testing.T, podName string, containerName string, remoteDir string) {
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

func (u *sshUt) kubevpnSyncWithServiceMesh(t *testing.T) {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	endpoint := u.kubevpnSync(t, ctx, true)
	u.healthChecker(t, endpoint, nil, remoteSyncOrigin)
	u.healthChecker(t, endpoint, map[string]string{"env": "test"}, remoteSyncPod)
}

func (u *sshUt) checkContent(ctx context.Context, t *testing.T, podName string, containerName string, remotePath string) {
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

func (u *sshUt) writeTempFile(t *testing.T) string {
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

func (u *sshUt) checkSyncWithFullProxyStatus(t *testing.T) {
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

func (u *sshUt) kubevpnUnSync(t *testing.T) {
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

func (u *sshUt) checkSyncWithServiceMeshStatus(t *testing.T) {
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
