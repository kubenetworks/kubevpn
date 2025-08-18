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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

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

func kubevpnSyncWithFullProxy(t *testing.T) {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	endpoint := kubevpnSync(t, ctx, false)
	healthChecker(t, endpoint, nil, remoteSyncPod)
	healthChecker(t, endpoint, map[string]string{"env": "test"}, remoteSyncPod)
}

func kubevpnSync(t *testing.T, ctx context.Context, isServiceMesh bool) string {
	path := writeTempFile(t)
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
	cmd := exec.Command("kubevpn", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}

	list, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("origin-workload", "authors").String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) == 0 {
		t.Fatal("expect at least one pod")
	}

	remotePath := fmt.Sprintf("%s/%s", remoteDir, name)
	containerName := "authors"
	checkContent(t, list.Items[0].Name, containerName, remotePath)
	go execServer(ctx, t, list.Items[0].Name, containerName, remotePath)

	endpoint := fmt.Sprintf("http://%s:%v/health", list.Items[0].Status.PodIP, 9080)
	healthChecker(t, endpoint, nil, remoteSyncPod)
	app := "authors"
	ip, err := getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint = fmt.Sprintf("http://%s:%v/health", ip, 9080)
	return endpoint
}

func execServer(ctx context.Context, t *testing.T, podName string, containerName string, remoteDir string) {
	for ctx.Err() == nil {
		output, err := util.Shell(
			ctx,
			clientset,
			restconfig,
			podName,
			containerName,
			namespace,
			[]string{"go", "run", remoteDir},
		)
		if err != nil {
			t.Log(err, output)
		}
		time.Sleep(time.Second * 1)
	}
}

func kubevpnSyncWithServiceMesh(t *testing.T) {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	endpoint := kubevpnSync(t, ctx, true)
	healthChecker(t, endpoint, nil, remoteSyncOrigin)
	healthChecker(t, endpoint, map[string]string{"env": "test"}, remoteSyncPod)
}

func checkContent(t *testing.T, podName string, containerName string, remotePath string) {
	err := retry.OnError(
		wait.Backoff{Duration: time.Second, Factor: 1, Jitter: 0, Steps: 120},
		func(err error) bool { return err != nil },
		func() error {
			shell, err := util.Shell(
				context.Background(),
				clientset,
				restconfig,
				podName,
				containerName,
				namespace,
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
	writeTempFile(t)
}

func writeTempFile(t *testing.T) string {
	tempDir := t.TempDir()
	subDir := filepath.Join(tempDir, "code")
	err := os.Mkdir(subDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(subDir, "main.go")
	temp, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	_, err = temp.WriteString(content)
	if err != nil {
		t.Fatal(err)
	}
	err = temp.Close()
	if err != nil {
		t.Fatal(err)
	}
	return temp.Name()
}

func checkSyncWithFullProxyStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(output))
	}

	expect := status{List: []*connection{{
		Namespace: namespace,
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: namespace,
			Workload:  "deployments.apps/authors",
			RuleList: []*proxyRule{{
				Headers:       nil,
				CurrentDevice: false,
				PortMap:       map[int32]int32{9080: 9080, 80: 80},
			}},
		}},
		SyncList: []*syncItem{{
			Namespace: namespace,
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
	cmd = exec.Command("kubevpn", "unsync", expect.List[0].SyncList[0].RuleList[0].DstWorkload)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func checkSyncWithServiceMeshStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(output))
	}

	expect := status{List: []*connection{{
		Namespace: namespace,
		Status:    "connected",
		ProxyList: []*proxyItem{{
			Namespace: namespace,
			Workload:  "deployments.apps/authors",
			RuleList: []*proxyRule{{
				Headers:       map[string]string{"env": "test"},
				CurrentDevice: false,
				PortMap:       map[int32]int32{9080: 9080, 80: 80},
			}},
		}},
		SyncList: []*syncItem{{
			Namespace: namespace,
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
	cmd = exec.Command("kubevpn", "unsync", expect.List[0].SyncList[0].RuleList[0].DstWorkload)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}
