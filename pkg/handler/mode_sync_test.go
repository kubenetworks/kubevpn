package handler

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"testing"
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

func kubevpnSync(t *testing.T) {
	path := writeTempFile(t)
	remotePath := "/app/test.go"
	cmd := exec.Command("kubevpn", "sync", "deploy/authors", "--sync", fmt.Sprintf("%s:%s", path, remotePath))
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(output))
	}

}

func writeTempFile(t *testing.T) string {
	temp, err := os.CreateTemp("", "main.go")
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

func checkSyncStatus(t *testing.T) {
	cmd := exec.Command("kubevpn", "status", "-o", "json")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
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
				PortMap:       map[int32]int32{9080: 9080},
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

	expect.List[0].SyncList[0].RuleList[0].DstWorkload = statuses.List[0].SyncList[0].RuleList[0].DstWorkload

	if !reflect.DeepEqual(statuses, expect) {
		marshal, _ := json.Marshal(expect)
		t.Fatalf("expect: %s, but was: %s", string(marshal), string(output))
	}
}
