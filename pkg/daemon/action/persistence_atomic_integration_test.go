package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// TestIntegration_PersistedConfigAtomicLifecycle drives registry changes and
// concurrent persisted-state writes while readers continuously decode the DB.
func TestIntegration_PersistedConfigAtomicLifecycle(t *testing.T) {
	preserveDBFile(t)
	if err := os.Remove(config.GetDBPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clear persisted state: %v", err)
	}
	svr := &Server{}
	readErr := make(chan error, 1)
	writeErr := make(chan error, 2)
	done := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-done:
				return
			default:
			}
			data, err := os.ReadFile(config.GetDBPath())
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				readErr <- err
				return
			}
			jsonData, err := yaml.YAMLToJSON(data)
			if err != nil {
				readErr <- err
				return
			}
			var conf Config
			if err = json.Unmarshal(jsonData, &conf); err != nil {
				readErr <- err
				return
			}
		}
	}()

	var writers sync.WaitGroup
	writers.Add(2)
	go func() {
		defer writers.Done()
		for i := 0; i < 40; i++ {
			id := fmt.Sprintf("connection-%d", i)
			svr.registry.add(&handler.ConnectOptions{
				ConnectionID: id,
				RequestRaw: mustMarshalConnectRequest(&rpc.ConnectRequest{
					Namespace:       "default",
					KubeconfigBytes: "apiVersion: v1",
				}),
			}, true)
			if err := svr.OffloadToConfig(); err != nil {
				writeErr <- err
				return
			}
			svr.registry.removeByID(id)
		}
	}()
	go func() {
		defer writers.Done()
		for i := 0; i < 40; i++ {
			if err := svr.OffloadToConfig(); err != nil {
				writeErr <- err
				return
			}
		}
	}()
	writers.Wait()
	close(done)
	<-readerDone
	select {
	case err := <-readErr:
		t.Fatalf("reader observed invalid persisted state: %v", err)
	default:
	}
	select {
	case err := <-writeErr:
		t.Fatalf("persist registry snapshot: %v", err)
	default:
	}

	// Publish a known final registry and verify a full read-modify-replay cycle.
	svr.registry.takeAll()
	for _, id := range []string{"final-a", "final-b"} {
		svr.registry.add(&handler.ConnectOptions{
			ConnectionID: id,
			RequestRaw: mustMarshalConnectRequest(&rpc.ConnectRequest{
				Namespace:       id,
				KubeconfigBytes: "apiVersion: v1",
			}),
		}, true)
	}
	if err := svr.OffloadToConfig(); err != nil {
		t.Fatalf("persist final registry: %v", err)
	}
	data, err := os.ReadFile(config.GetDBPath())
	if err != nil {
		t.Fatalf("read final registry: %v", err)
	}
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		t.Fatalf("decode final YAML: %v", err)
	}
	var conf Config
	if err = json.Unmarshal(jsonData, &conf); err != nil {
		t.Fatalf("decode final registry: %v", err)
	}
	if len(conf.SecondaryConnect) != 2 {
		t.Fatalf("final registry contains %d connections, want 2", len(conf.SecondaryConnect))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(config.GetDBPath())
		if err != nil {
			t.Fatalf("stat final registry: %v", err)
		}
		if got := info.Mode().Perm(); got != config.FileModePrivateFile {
			t.Fatalf("DB mode = %o, want %o", got, config.FileModePrivateFile)
		}
	}
}

func TestWriteFileAtomically_RenameFailurePreservesPreviousFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db")
	const previous = "previous-complete-state"
	if err := os.WriteFile(path, []byte(previous), 0600); err != nil {
		t.Fatalf("write previous state: %v", err)
	}
	originalRename := renameFile
	renameFile = func(string, string) error { return errors.New("injected rename failure") }
	t.Cleanup(func() { renameFile = originalRename })

	err := writeFileAtomically(path, []byte("partial-new-state"), 0600)
	if err == nil || !strings.Contains(err.Error(), "injected rename failure") {
		t.Fatalf("expected injected rename failure, got %v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || string(data) != previous {
		t.Fatalf("previous state was not preserved: data=%q, err=%v", data, readErr)
	}
	matches, globErr := filepath.Glob(filepath.Join(dir, ".db.tmp-*"))
	if globErr != nil {
		t.Fatalf("find temporary files: %v", globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain after failure: %v", matches)
	}
}
