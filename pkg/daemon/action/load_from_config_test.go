package action

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// saveRestoreDB saves the current DB file and restores it (or removes it) after
// the test. This mirrors the pattern used by the OffloadToConfig tests.
func saveRestoreDB(t *testing.T) {
	t.Helper()
	dbPath := config.GetDBPath()
	origData, origErr := os.ReadFile(dbPath)
	t.Cleanup(func() {
		if origErr == nil {
			_ = os.WriteFile(dbPath, origData, 0644)
		} else {
			_ = os.Remove(dbPath)
		}
	})
}

// writeDBFile writes content to the DB path used by LoadFromConfig.
func writeDBFile(t *testing.T, content []byte) {
	t.Helper()
	if err := os.WriteFile(config.GetDBPath(), content, 0644); err != nil {
		t.Fatalf("setup: writing DB file: %v", err)
	}
}

// validEmptyConfigYAML produces a well-formed YAML config with empty SecondaryConnect.
func validEmptyConfigYAML(t *testing.T) []byte {
	t.Helper()
	conf := &Config{SecondaryConnect: nil}
	j, err := json.Marshal(conf)
	if err != nil {
		t.Fatalf("setup: json.Marshal: %v", err)
	}
	y, err := yaml.JSONToYAML(j)
	if err != nil {
		t.Fatalf("setup: yaml.JSONToYAML: %v", err)
	}
	return y
}

// validConfigWithConnectionYAML produces a well-formed YAML config with one
// SecondaryConnect entry containing a serialized ConnectRequest.
func validConfigWithConnectionYAML(t *testing.T) []byte {
	t.Helper()
	raw, err := proto.Marshal(&rpc.ConnectRequest{
		Namespace: "test-ns",
	})
	if err != nil {
		t.Fatalf("setup: proto.Marshal: %v", err)
	}
	conf := &Config{
		SecondaryConnect: []*handler.ConnectOptions{
			{RequestRaw: raw},
		},
	}
	j, err := json.Marshal(conf)
	if err != nil {
		t.Fatalf("setup: json.Marshal: %v", err)
	}
	y, err := yaml.JSONToYAML(j)
	if err != nil {
		t.Fatalf("setup: yaml.JSONToYAML: %v", err)
	}
	return y
}

// TestLoadFromConfig_FileNotExist verifies that LoadFromConfig returns an error
// (specifically os.ErrNotExist) when the DB file does not exist.
func TestLoadFromConfig_FileNotExist(t *testing.T) {
	saveRestoreDB(t)

	// Ensure the file does not exist.
	_ = os.Remove(config.GetDBPath())

	svr := &Server{}
	err := svr.LoadFromConfig(context.Background())
	if err == nil {
		t.Fatal("expected error when DB file does not exist, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got: %v", err)
	}
}

// TestLoadFromConfig_MalformedYAML verifies that LoadFromConfig returns an error
// when the DB file contains content that cannot be parsed as valid YAML/JSON.
func TestLoadFromConfig_MalformedYAML(t *testing.T) {
	saveRestoreDB(t)

	// Write syntactically broken content. sigs.k8s.io/yaml converts via JSON,
	// so content that is neither valid YAML nor valid JSON triggers an error.
	writeDBFile(t, []byte("{{{{not valid yaml: [[["))

	svr := &Server{}
	err := svr.LoadFromConfig(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
}

// TestLoadFromConfig_TypeMismatch verifies that LoadFromConfig returns an error
// when the file is valid YAML but the JSON form cannot be unmarshalled into
// Config (wrong type for SecondaryConnect field).
func TestLoadFromConfig_TypeMismatch(t *testing.T) {
	saveRestoreDB(t)

	// Valid YAML, but SecondaryConnect is a string instead of an array.
	writeDBFile(t, []byte("SecondaryConnect: \"not-an-array\"\n"))

	svr := &Server{}
	err := svr.LoadFromConfig(context.Background())
	if err == nil {
		t.Fatal("expected error for type-mismatched JSON, got nil")
	}
}

// TestLoadFromConfig_EmptySecondaryConnect verifies the early-exit path:
// when SecondaryConnect is empty LoadFromConfig returns nil immediately without
// touching svr.connections or calling GetClient.
func TestLoadFromConfig_EmptySecondaryConnect(t *testing.T) {
	saveRestoreDB(t)
	writeDBFile(t, validEmptyConfigYAML(t))

	getClientCalled := false
	svr := &Server{
		GetClient: func(isSudo bool) (rpc.DaemonClient, error) {
			getClientCalled = true
			t.Error("GetClient was called unexpectedly for empty SecondaryConnect")
			return nil, nil
		},
	}

	err := svr.LoadFromConfig(context.Background())
	if err != nil {
		t.Fatalf("expected nil error for empty SecondaryConnect, got: %v", err)
	}
	if getClientCalled {
		t.Error("GetClient must not be called when SecondaryConnect is empty")
	}

	svr.connMu.RLock()
	connCount := len(svr.connections)
	svr.connMu.RUnlock()
	if connCount != 0 {
		t.Errorf("expected 0 connections after early-exit, got %d", connCount)
	}
}

// TestLoadFromConfig_CtxCancelledWithConnections verifies the ctx-cancel branch:
// when the context is already cancelled before LoadFromConfig is called and
// SecondaryConnect is non-empty, the function exits in O(1) time without
// sleeping and returns context.Canceled.
//
// The loop in LoadFromConfig is:
//
//	for ctx.Err() == nil { ... }
//	if ctx.Err() != nil { return ctx.Err() }
//
// With a pre-cancelled context ctx.Err() == context.Canceled is true immediately,
// so the loop body (and time.Sleep) is never entered. The return is instant.
func TestLoadFromConfig_CtxCancelledWithConnections(t *testing.T) {
	saveRestoreDB(t)
	writeDBFile(t, validConfigWithConnectionYAML(t))

	// Pre-cancel the context before passing it to LoadFromConfig.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	getClientCalled := false
	svr := &Server{
		GetClient: func(isSudo bool) (rpc.DaemonClient, error) {
			// This must never be reached for a pre-cancelled context.
			getClientCalled = true
			t.Error("GetClient was called despite ctx already being cancelled")
			return nil, nil
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- svr.LoadFromConfig(ctx)
	}()

	// LoadFromConfig must complete well within 2 seconds for a pre-cancelled
	// context. In practice it returns in microseconds.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-nil error (context.Canceled) when ctx is pre-cancelled, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LoadFromConfig did not return within 2 s for a pre-cancelled context — potential hang")
	}

	if getClientCalled {
		t.Error("GetClient must not be called when context is already cancelled")
	}
}
