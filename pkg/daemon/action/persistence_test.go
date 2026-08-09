package action

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"google.golang.org/protobuf/proto"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

func mustMarshalConnectRequest(req *rpc.ConnectRequest) []byte {
	b, err := proto.Marshal(req)
	if err != nil {
		panic(err)
	}
	return b
}

func TestServer_OffloadToConfig(t *testing.T) {
	// Clean up any pre-existing DB file and restore state after test
	dbPath := config.GetDBPath()
	origData, origErr := os.ReadFile(dbPath)
	t.Cleanup(func() {
		if origErr == nil {
			_ = os.WriteFile(dbPath, origData, 0644)
		} else {
			_ = os.Remove(dbPath)
		}
	})

	svr := &Server{
		connections: []handler.Connection{
			&handler.ConnectOptions{
				RequestRaw: mustMarshalConnectRequest(&rpc.ConnectRequest{
					Namespace:            "default",
					OriginKubeconfigPath: "/home/user/.kube/config",
				}),
			},
			&handler.ConnectOptions{
				RequestRaw: mustMarshalConnectRequest(&rpc.ConnectRequest{
					Namespace: "staging",
					Image:     "ghcr.io/kubenetworks/kubevpn:v2.0.0",
				}),
			},
		},
	}

	err := svr.OffloadToConfig()
	if err != nil {
		t.Fatalf("OffloadToConfig: %v", err)
	}

	// Verify file was written
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("reading DB file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("DB file is empty")
	}

	// Verify it is valid YAML that can be converted to JSON
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		t.Fatalf("YAML->JSON conversion: %v", err)
	}

	var conf Config
	err = json.Unmarshal(jsonData, &conf)
	if err != nil {
		t.Fatalf("JSON unmarshal: %v", err)
	}

	if len(conf.SecondaryConnect) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(conf.SecondaryConnect))
	}

	// Verify first connection fields
	c0 := conf.SecondaryConnect[0]
	if len(c0.RequestRaw) == 0 {
		t.Fatal("first connection RequestRaw is empty")
	}
	var req0 rpc.ConnectRequest
	if err := proto.Unmarshal(c0.RequestRaw, &req0); err != nil {
		t.Fatalf("unmarshal first RequestRaw: %v", err)
	}
	if req0.Namespace != "default" {
		t.Errorf("expected namespace 'default', got %q", req0.Namespace)
	}
	if req0.OriginKubeconfigPath != "/home/user/.kube/config" {
		t.Errorf("unexpected kubeconfig path: %q", req0.OriginKubeconfigPath)
	}
	// Verify second connection
	c1 := conf.SecondaryConnect[1]
	if len(c1.RequestRaw) == 0 {
		t.Fatal("second connection RequestRaw is empty")
	}
	var req1 rpc.ConnectRequest
	if err := proto.Unmarshal(c1.RequestRaw, &req1); err != nil {
		t.Fatalf("unmarshal second RequestRaw: %v", err)
	}
	if req1.Namespace != "staging" {
		t.Errorf("expected namespace 'staging', got %q", req1.Namespace)
	}
	if req1.Image != "ghcr.io/kubenetworks/kubevpn:v2.0.0" {
		t.Errorf("unexpected image: %q", req1.Image)
	}
}

func TestServer_OffloadToConfig_Empty(t *testing.T) {
	dbPath := config.GetDBPath()
	origData, origErr := os.ReadFile(dbPath)
	t.Cleanup(func() {
		if origErr == nil {
			_ = os.WriteFile(dbPath, origData, 0644)
		} else {
			_ = os.Remove(dbPath)
		}
	})

	svr := &Server{
		connections: nil,
	}

	err := svr.OffloadToConfig()
	if err != nil {
		t.Fatalf("OffloadToConfig (empty): %v", err)
	}

	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("reading DB file: %v", err)
	}

	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		t.Fatalf("YAML->JSON: %v", err)
	}

	var conf Config
	err = json.Unmarshal(jsonData, &conf)
	if err != nil {
		t.Fatalf("JSON unmarshal: %v", err)
	}

	if len(conf.SecondaryConnect) != 0 {
		t.Fatalf("expected 0 connections, got %d", len(conf.SecondaryConnect))
	}
}

func TestServer_CleanupConfig(t *testing.T) {
	dbPath := config.GetDBPath()
	origData, origErr := os.ReadFile(dbPath)
	t.Cleanup(func() {
		if origErr == nil {
			_ = os.WriteFile(dbPath, origData, 0644)
		} else {
			_ = os.Remove(dbPath)
		}
	})

	// Create a file first so CleanupConfig has something to remove
	err := os.WriteFile(dbPath, []byte("test-data"), 0644)
	if err != nil {
		t.Fatalf("setup: writing test file: %v", err)
	}

	svr := &Server{}
	err = svr.CleanupConfig()
	if err != nil {
		t.Fatalf("CleanupConfig: %v", err)
	}

	// Verify file is removed
	_, err = os.Stat(dbPath)
	if !os.IsNotExist(err) {
		t.Fatalf("expected file to be removed, got stat err: %v", err)
	}
}

func TestServer_CleanupConfig_NoFile(t *testing.T) {
	dbPath := config.GetDBPath()
	origData, origErr := os.ReadFile(dbPath)
	t.Cleanup(func() {
		if origErr == nil {
			_ = os.WriteFile(dbPath, origData, 0644)
		} else {
			_ = os.Remove(dbPath)
		}
	})

	// Ensure file does not exist
	_ = os.Remove(dbPath)

	svr := &Server{}
	err := svr.CleanupConfig()
	if err == nil {
		t.Fatal("expected error when file does not exist")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("expected os.ErrNotExist, got: %v", err)
	}
}

func TestConfig_SerializationRoundTrip(t *testing.T) {

	original := &Config{
		SecondaryConnect: []*handler.ConnectOptions{
			{
				RequestRaw: mustMarshalConnectRequest(&rpc.ConnectRequest{
					KubeconfigBytes:      "apiVersion: v1\nclusters: []",
					Namespace:            "production",
					OriginKubeconfigPath: "/etc/kube/config",
					Image:                "ghcr.io/kubenetworks/kubevpn:v2.1.0",
					Foreground:           true,
					Level:                3,
				}),
			},
		},
	}

	// Marshal to JSON
	jsonData, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("JSON marshal: %v", err)
	}

	// Convert to YAML (same path as OffloadToConfig)
	yamlData, err := yaml.JSONToYAML(jsonData)
	if err != nil {
		t.Fatalf("JSON->YAML: %v", err)
	}

	// Convert YAML back to JSON (same path as LoadFromConfig)
	jsonBack, err := yaml.YAMLToJSON(yamlData)
	if err != nil {
		t.Fatalf("YAML->JSON: %v", err)
	}

	// Unmarshal back
	var restored Config
	err = json.Unmarshal(jsonBack, &restored)
	if err != nil {
		t.Fatalf("JSON unmarshal: %v", err)
	}

	if len(restored.SecondaryConnect) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(restored.SecondaryConnect))
	}

	c := restored.SecondaryConnect[0]
	if len(c.RequestRaw) == 0 {
		t.Fatal("restored RequestRaw is empty")
	}
	var req rpc.ConnectRequest
	if err := proto.Unmarshal(c.RequestRaw, &req); err != nil {
		t.Fatalf("unmarshal RequestRaw: %v", err)
	}
	if req.Namespace != "production" {
		t.Errorf("namespace mismatch: got %q", req.Namespace)
	}
	if req.OriginKubeconfigPath != "/etc/kube/config" {
		t.Errorf("kubeconfig path mismatch: got %q", req.OriginKubeconfigPath)
	}
	if req.Image != "ghcr.io/kubenetworks/kubevpn:v2.1.0" {
		t.Errorf("image mismatch: got %q", req.Image)
	}
	if !req.Foreground {
		t.Error("expected Foreground=true")
	}
	if req.Level != 3 {
		t.Errorf("level mismatch: got %d", req.Level)
	}
}

func TestConfig_SerializationRoundTrip_NilFields(t *testing.T) {
	original := &Config{
		SecondaryConnect: []*handler.ConnectOptions{
			{
				RequestRaw: nil,
			},
		},
	}

	jsonData, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("JSON marshal: %v", err)
	}

	yamlData, err := yaml.JSONToYAML(jsonData)
	if err != nil {
		t.Fatalf("JSON->YAML: %v", err)
	}

	jsonBack, err := yaml.YAMLToJSON(yamlData)
	if err != nil {
		t.Fatalf("YAML->JSON: %v", err)
	}

	var restored Config
	err = json.Unmarshal(jsonBack, &restored)
	if err != nil {
		t.Fatalf("JSON unmarshal: %v", err)
	}

	if len(restored.SecondaryConnect) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(restored.SecondaryConnect))
	}

	c := restored.SecondaryConnect[0]
	if len(c.RequestRaw) != 0 {
		t.Errorf("expected empty RequestRaw, got %v", c.RequestRaw)
	}
}

func TestServer_ConnectionList_Empty(t *testing.T) {
	svr := &Server{}
	resp, err := svr.ConnectionList(context.Background(), &rpc.ConnectionListRequest{})
	if err != nil {
		t.Fatalf("ConnectionList: %v", err)
	}
	if len(resp.List) != 0 {
		t.Fatalf("expected 0 items, got %d", len(resp.List))
	}
	if resp.CurrentConnectionID != "" {
		t.Errorf("expected empty currentConnectionID, got %q", resp.CurrentConnectionID)
	}
}

func TestServer_ConnectionList_WithConnections(t *testing.T) {
	// buildConnectionStatus calls GetFactory() which requires a properly initialized factory.
	// We use a temp kubeconfig to create a real cmdutil.Factory for testing.
	tmpKubeconfig := t.TempDir() + "/kubeconfig"
	kubeconfigContent := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    namespace: default
  name: test-context
current-context: test-context
`
	if err := os.WriteFile(tmpKubeconfig, []byte(kubeconfigContent), 0644); err != nil {
		t.Fatalf("writing temp kubeconfig: %v", err)
	}

	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = &tmpKubeconfig
	ns := "default"
	configFlags.Namespace = &ns
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	factory := cmdutil.NewFactory(matchVersionFlags)

	conn1 := handler.NewConnectOptionsForTest(factory, tmpKubeconfig, "default")
	conn2 := handler.NewConnectOptionsForTest(factory, tmpKubeconfig, "staging")

	svr := &Server{
		currentConnectionID: "conn-abc",
		connections:         []handler.Connection{conn1, conn2},
	}

	resp, err := svr.ConnectionList(context.Background(), &rpc.ConnectionListRequest{})
	if err != nil {
		t.Fatalf("ConnectionList: %v", err)
	}
	if len(resp.List) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp.List))
	}
	if resp.CurrentConnectionID != "conn-abc" {
		t.Errorf("expected currentConnectionID 'conn-abc', got %q", resp.CurrentConnectionID)
	}

	// Verify fields from buildConnectionStatus — kubeconfig and namespace are passed through
	if resp.List[0].Kubeconfig != tmpKubeconfig {
		t.Errorf("list[0] kubeconfig: got %q", resp.List[0].Kubeconfig)
	}
	if resp.List[0].Namespace != "default" {
		t.Errorf("list[0] namespace: got %q", resp.List[0].Namespace)
	}
	if resp.List[0].Cluster != "test-cluster" {
		t.Errorf("list[0] cluster: got %q", resp.List[0].Cluster)
	}
	if resp.List[1].Kubeconfig != tmpKubeconfig {
		t.Errorf("list[1] kubeconfig: got %q", resp.List[1].Kubeconfig)
	}
	if resp.List[1].Namespace != "staging" {
		t.Errorf("list[1] namespace: got %q", resp.List[1].Namespace)
	}
}

func TestServer_ConnectionUse_NotFound(t *testing.T) {
	svr := &Server{
		connections: []handler.Connection{
			&handler.ConnectOptions{ManagerNamespace: "ns1"},
		},
	}

	_, err := svr.ConnectionUse(context.Background(), &rpc.ConnectionUseRequest{
		ConnectionID: "nonexistent-id",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent connection")
	}
	if err.Error() != "no connection found" {
		t.Errorf("unexpected error message: %q", err.Error())
	}
}

func TestServer_ConnectionUse_EmptyConnections(t *testing.T) {
	svr := &Server{}
	_, err := svr.ConnectionUse(context.Background(), &rpc.ConnectionUseRequest{
		ConnectionID: "any-id",
	})
	if err == nil {
		t.Fatal("expected error for empty connections")
	}
}

func TestServer_ConnectionList_CurrentConnectionID(t *testing.T) {
	// Verify that currentConnectionID is returned in the response even when
	// no connections exist (the field is independent of the list).
	svr := &Server{
		currentConnectionID: "my-conn-id-123",
	}
	resp, err := svr.ConnectionList(context.Background(), &rpc.ConnectionListRequest{})
	if err != nil {
		t.Fatalf("ConnectionList: %v", err)
	}
	if resp.CurrentConnectionID != "my-conn-id-123" {
		t.Errorf("expected currentConnectionID 'my-conn-id-123', got %q", resp.CurrentConnectionID)
	}
}

func TestServer_OffloadToConfig_RoundTrip(t *testing.T) {
	dbPath := config.GetDBPath()
	origData, origErr := os.ReadFile(dbPath)
	t.Cleanup(func() {
		if origErr == nil {
			_ = os.WriteFile(dbPath, origData, 0644)
		} else {
			_ = os.Remove(dbPath)
		}
	})

	svr := &Server{
		connections: []handler.Connection{
			&handler.ConnectOptions{
				RequestRaw: mustMarshalConnectRequest(&rpc.ConnectRequest{
					Namespace:            "production",
					OriginKubeconfigPath: "/tmp/kubeconfig",
					Image:                "ghcr.io/kubenetworks/kubevpn:test",
				}),
			},
		},
	}

	// Offload to disk
	if err := svr.OffloadToConfig(); err != nil {
		t.Fatalf("OffloadToConfig: %v", err)
	}

	// Load back from disk into a Config struct (same path as LoadFromConfig uses)
	content, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("reading DB file: %v", err)
	}
	jsonData, err := yaml.YAMLToJSON(content)
	if err != nil {
		t.Fatalf("YAML->JSON: %v", err)
	}
	var conf Config
	if err := json.Unmarshal(jsonData, &conf); err != nil {
		t.Fatalf("JSON unmarshal: %v", err)
	}

	// Verify connections preserved
	if len(conf.SecondaryConnect) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(conf.SecondaryConnect))
	}
	c := conf.SecondaryConnect[0]
	if len(c.RequestRaw) == 0 {
		t.Fatal("restored RequestRaw is empty")
	}
	var req rpc.ConnectRequest
	if err := proto.Unmarshal(c.RequestRaw, &req); err != nil {
		t.Fatalf("unmarshal RequestRaw: %v", err)
	}
	if req.Namespace != "production" {
		t.Errorf("namespace: got %q, want 'production'", req.Namespace)
	}
	if req.OriginKubeconfigPath != "/tmp/kubeconfig" {
		t.Errorf("kubeconfig path: got %q", req.OriginKubeconfigPath)
	}
	if req.Image != "ghcr.io/kubenetworks/kubevpn:test" {
		t.Errorf("image: got %q", req.Image)
	}
}
