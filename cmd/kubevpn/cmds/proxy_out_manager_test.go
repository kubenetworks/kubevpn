//go:build !windows

package cmds

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func TestManagedProxyInfoRoundTrip(t *testing.T) {
	tempDir := t.TempDir()
	oldStateDir := proxyOutStateDirFunc
	oldLogPath := proxyOutLogPathFunc
	proxyOutStateDirFunc = func() string { return tempDir }
	proxyOutLogPathFunc = func(connectionID string) string { return filepath.Join(tempDir, connectionID+".log") }
	defer func() {
		proxyOutStateDirFunc = oldStateDir
		proxyOutLogPathFunc = oldLogPath
	}()

	info := managedProxyInfo{
		ConnectionID: "abc123",
		PID:          12345,
		SocksAddress: "127.0.0.1:1080",
		Namespace:    "kubevpn",
	}
	if err := writeManagedProxyInfo(info); err != nil {
		t.Fatal(err)
	}
	got, err := readManagedProxyInfo("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got.ConnectionID != info.ConnectionID || got.PID != info.PID || got.SocksAddress != info.SocksAddress {
		t.Fatalf("unexpected managed proxy info: %#v", got)
	}
}

func TestStopManagedProxyRemovesStateAndKubeconfig(t *testing.T) {
	tempDir := t.TempDir()
	oldStateDir := proxyOutStateDirFunc
	oldLogPath := proxyOutLogPathFunc
	proxyOutStateDirFunc = func() string { return tempDir }
	proxyOutLogPathFunc = func(connectionID string) string { return filepath.Join(tempDir, connectionID+".log") }
	defer func() {
		proxyOutStateDirFunc = oldStateDir
		proxyOutLogPathFunc = oldLogPath
	}()

	kubeconfigPath := filepath.Join(tempDir, "proxy.kubeconfig")
	if _, err := util.ConvertToTempKubeconfigFile([]byte(`{"apiVersion":"v1","kind":"Config","clusters":[],"contexts":[],"users":[]}`), kubeconfigPath); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedProxyInfo(managedProxyInfo{
		ConnectionID:   "conn1",
		PID:            999999,
		KubeconfigPath: kubeconfigPath,
	}); err != nil {
		t.Fatal(err)
	}

	if err := stopManagedProxy("conn1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(proxyOutStatePath("conn1")); !os.IsNotExist(err) {
		t.Fatalf("expected state file removed, got err=%v", err)
	}
	if _, err := os.Stat(kubeconfigPath); !os.IsNotExist(err) {
		t.Fatalf("expected kubeconfig removed, got err=%v", err)
	}
}

func TestStartManagedSocksProxyWritesState(t *testing.T) {
	tempDir := t.TempDir()
	oldStateDir := proxyOutStateDirFunc
	oldLogPath := proxyOutLogPathFunc
	oldExec := execProxyOutCommand
	oldResolve := resolveCurrentExecutable
	proxyOutStateDirFunc = func() string { return tempDir }
	proxyOutLogPathFunc = func(connectionID string) string { return filepath.Join(tempDir, connectionID+".log") }
	resolveCurrentExecutable = func() (string, error) { return "/bin/sh", nil }
	execProxyOutCommand = func(name string, args ...string) *exec.Cmd {
		return exec.Command("/bin/sh", "-c", "sleep 30")
	}
	defer func() {
		proxyOutStateDirFunc = oldStateDir
		proxyOutLogPathFunc = oldLogPath
		execProxyOutCommand = oldExec
		resolveCurrentExecutable = oldResolve
	}()

	if err := startManagedSocksProxy("conn2", []byte(`{"apiVersion":"v1","kind":"Config","clusters":[],"contexts":[],"users":[]}`), "kubevpn", "127.0.0.1:1089"); err != nil {
		t.Fatal(err)
	}
	info, err := readManagedProxyInfo("conn2")
	if err != nil {
		t.Fatal(err)
	}
	if info.PID <= 0 || info.SocksAddress != "127.0.0.1:1089" {
		t.Fatalf("unexpected managed proxy info: %#v", info)
	}
	_ = stopManagedProxy("conn2")
}

func TestStartManagedSocksProxyReplacesPreviousState(t *testing.T) {
	tempDir := t.TempDir()
	oldStateDir := proxyOutStateDirFunc
	oldLogPath := proxyOutLogPathFunc
	oldExec := execProxyOutCommand
	oldResolve := resolveCurrentExecutable
	proxyOutStateDirFunc = func() string { return tempDir }
	proxyOutLogPathFunc = func(connectionID string) string { return filepath.Join(tempDir, connectionID+".log") }
	resolveCurrentExecutable = func() (string, error) { return "/bin/sh", nil }
	execProxyOutCommand = func(name string, args ...string) *exec.Cmd {
		return exec.Command("/bin/sh", "-c", "sleep 30")
	}
	defer func() {
		proxyOutStateDirFunc = oldStateDir
		proxyOutLogPathFunc = oldLogPath
		execProxyOutCommand = oldExec
		resolveCurrentExecutable = oldResolve
	}()

	staleKubeconfig := filepath.Join(tempDir, "stale.kubeconfig")
	if _, err := util.ConvertToTempKubeconfigFile([]byte(`{"apiVersion":"v1","kind":"Config","clusters":[],"contexts":[],"users":[]}`), staleKubeconfig); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedProxyInfo(managedProxyInfo{
		ConnectionID:   "conn-restart",
		PID:            999999,
		KubeconfigPath: staleKubeconfig,
	}); err != nil {
		t.Fatal(err)
	}

	if err := startManagedSocksProxy("conn-restart", []byte(`{"apiVersion":"v1","kind":"Config","clusters":[],"contexts":[],"users":[]}`), "kubevpn", "127.0.0.1:1090"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(staleKubeconfig); !os.IsNotExist(err) {
		t.Fatalf("expected stale kubeconfig removed, got err=%v", err)
	}
	info, err := readManagedProxyInfo("conn-restart")
	if err != nil {
		t.Fatal(err)
	}
	if info.PID <= 0 || info.SocksAddress != "127.0.0.1:1090" {
		t.Fatalf("unexpected managed proxy info after restart: %#v", info)
	}
	_ = stopManagedProxy("conn-restart")
}
