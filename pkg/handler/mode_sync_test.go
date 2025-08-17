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

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
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
	path := writeTempFile(t)
	name := filepath.Base(path)
	dir := filepath.Dir(path)
	remoteDir := "/app/test"
	cmd := exec.Command("kubevpn", "sync", "deploy/authors", "--debug", "--sync", fmt.Sprintf("%s:%s", dir, remoteDir))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	list, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("origin-workload", "authors").String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) == 0 {
		t.Fatal("expect at least one pod")
	}

	checkContent(t, list.Items[0].Name, fmt.Sprintf("%s/%s", remoteDir, name))
	go func() {
		_, err = util.Shell(
			ctx,
			clientset,
			restconfig,
			list.Items[0].Name,
			config.ContainerSidecarSyncthing,
			namespace,
			[]string{"go", "run", fmt.Sprintf("%s/%s", remoteDir, name)},
		)
		if err != nil {
			t.Log(err.Error())
		}
	}()

	app := "authors"
	ip, err := getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	healthChecker(t, endpoint, nil, remoteSyncPod)
	healthChecker(t, endpoint, map[string]string{"env": "test"}, remoteSyncPod)
}

func kubevpnSyncWithServiceMesh(t *testing.T) {
	path := writeTempFile(t)
	name := filepath.Base(path)
	dir := filepath.Dir(path)
	remoteDir := "/app/test"
	cmd := exec.Command("kubevpn", "sync", "deploy/authors", "--sync", fmt.Sprintf("%s:%s", dir, remoteDir), "--headers", "env=test", "--debug")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	list, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("origin-workload", "authors").String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) == 0 {
		t.Fatal("expect at least one pod")
	}

	checkContent(t, list.Items[0].Name, fmt.Sprintf("%s/%s", remoteDir, name))
	go func() {
		_, err = util.Shell(
			ctx,
			clientset,
			restconfig,
			list.Items[0].Name,
			config.ContainerSidecarSyncthing,
			namespace,
			[]string{"go", "run", fmt.Sprintf("%s/%s", remoteDir, name)},
		)
		if err != nil {
			t.Log(err.Error())
		}
	}()

	app := "authors"
	ip, err := getPodIP(app)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("http://%s:%v/health", ip, 9080)
	healthChecker(t, endpoint, nil, remoteSyncOrigin)
	healthChecker(t, endpoint, map[string]string{"env": "test"}, remoteSyncPod)
}

func checkContent(t *testing.T, podName string, remotePath string) {
	err := retry.OnError(
		wait.Backoff{Duration: time.Second, Factor: 1, Jitter: 0, Steps: 120},
		func(err error) bool { return err != nil },
		func() error {
			shell, err := util.Shell(context.Background(), clientset, restconfig, podName, config.ContainerSidecarSyncthing, namespace, []string{"cat", remotePath})
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

func TestName(t *testing.T) {
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
		Namespace: "kubevpn",
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
			Workload:  "deployments/authors",
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
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(output))
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
		Namespace: "kubevpn",
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
			Workload:  "deployments/authors",
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
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(output))
	}
	cmd = exec.Command("kubevpn", "unsync", expect.List[0].SyncList[0].RuleList[0].DstWorkload)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}
