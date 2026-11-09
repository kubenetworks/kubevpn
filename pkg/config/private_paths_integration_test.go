//go:build !windows

package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const privatePathsHelperEnv = "KUBEVPN_PRIVATE_PATHS_HELPER"

// TestIntegration_PrivatePathsMigrateAcrossRestarts uses two fresh processes so
// package init runs against the same HOME before and after legacy permissions.
func TestIntegration_PrivatePathsMigrateAcrossRestarts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root intentionally does not migrate user-controlled paths")
	}
	home := t.TempDir()
	for _, phase := range []string{"setup", "verify"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPrivatePathsHelperProcess$")
		cmd.Env = append(os.Environ(), "HOME="+home, privatePathsHelperEnv+"="+phase)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s process failed: %v\n%s", phase, err, output)
		}
	}
}

func TestPrivatePathsHelperProcess(t *testing.T) {
	phase := os.Getenv(privatePathsHelperEnv)
	if phase == "" {
		return
	}
	dirs := []string{homePath, daemonPath, logPath, GetPProfPath(), GetSyncthingPath(), GetTempPath()}
	switch phase {
	case "setup":
		_ = GetClientID()
		for _, path := range dirs {
			if err := os.Chmod(path, 0755); err != nil {
				t.Fatalf("make legacy directory %s: %v", path, err)
			}
		}
		for _, path := range []string{GetConfigFile(), filepath.Join(homePath, ClientIDFile)} {
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatalf("make legacy file %s: %v", path, err)
			}
		}
	case "verify":
		for _, path := range dirs {
			assertPrivatePathMode(t, path, FileModePrivateDir)
		}
		assertPrivatePathMode(t, GetConfigFile(), FileModePrivateFile)
		clientIDPath := filepath.Join(homePath, ClientIDFile)
		before, err := os.ReadFile(clientIDPath)
		if err != nil {
			t.Fatalf("read client ID before migration: %v", err)
		}
		if got := GetClientID(); got != string(before)[:12] {
			t.Fatalf("client ID changed across migration: got %q, stored %q", got, before)
		}
		assertPrivatePathMode(t, clientIDPath, FileModePrivateFile)
	default:
		t.Fatalf("unknown helper phase %q", phase)
	}
}

func assertPrivatePathMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %s, want %s", path, fmt.Sprintf("%#o", got), fmt.Sprintf("%#o", want))
	}
}
