package cmds

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type managedProxyInfo struct {
	ConnectionID   string    `json:"connectionID"`
	PID            int       `json:"pid"`
	SocksAddress   string    `json:"socksAddress"`
	KubeconfigPath string    `json:"kubeconfigPath"`
	Namespace      string    `json:"namespace"`
	LogPath        string    `json:"logPath"`
	StartedAt      time.Time `json:"startedAt"`
}

var execProxyOutCommand = exec.Command
var resolveCurrentExecutable = os.Executable
var proxyOutStateDirFunc = func() string {
	return filepath.Join(filepath.Dir(config.GetDBPath()), "proxy-out")
}
var proxyOutLogPathFunc = func(connectionID string) string {
	return filepath.Join(filepath.Dir(config.GetDaemonLogPath(false)), fmt.Sprintf("proxy-out-%s.log", connectionID))
}

func proxyOutStateDir() string {
	return proxyOutStateDirFunc()
}

func proxyOutStatePath(connectionID string) string {
	return filepath.Join(proxyOutStateDir(), connectionID+".json")
}

func proxyOutLogPath(connectionID string) string {
	return proxyOutLogPathFunc(connectionID)
}

func ensureProxyOutStateDir() error {
	return os.MkdirAll(proxyOutStateDir(), 0755)
}

func startManagedSocksProxy(connectionID string, kubeconfigBytes []byte, namespace, listenAddr string) error {
	if connectionID == "" {
		return fmt.Errorf("connection ID is required")
	}
	if listenAddr == "" {
		listenAddr = "127.0.0.1:1080"
	}
	if err := ensureProxyOutStateDir(); err != nil {
		return err
	}
	_ = stopManagedProxy(connectionID)

	kubeconfigPath := filepath.Join(config.GetTempPath(), fmt.Sprintf("proxy-out-%s.kubeconfig", connectionID))
	if _, err := util.ConvertToTempKubeconfigFile(kubeconfigBytes, kubeconfigPath); err != nil {
		return err
	}

	logPath := proxyOutLogPath(connectionID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		_ = os.Remove(kubeconfigPath)
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		_ = os.Remove(kubeconfigPath)
		return err
	}
	defer logFile.Close()

	exe, err := resolveCurrentExecutable()
	if err != nil {
		return err
	}

	args := []string{
		"--kubeconfig", kubeconfigPath,
		"--namespace", namespace,
		"proxy-out",
		"--listen-socks", listenAddr,
	}
	cmd := execProxyOutCommand(exe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.Env = os.Environ()
	detachProcess(cmd)

	if err := cmd.Start(); err != nil {
		_ = os.Remove(kubeconfigPath)
		return err
	}

	info := managedProxyInfo{
		ConnectionID:   connectionID,
		PID:            cmd.Process.Pid,
		SocksAddress:   listenAddr,
		KubeconfigPath: kubeconfigPath,
		Namespace:      namespace,
		LogPath:        logPath,
		StartedAt:      time.Now(),
	}
	if err := writeManagedProxyInfo(info); err != nil {
		_ = cmd.Process.Kill()
		_ = os.Remove(kubeconfigPath)
		return err
	}
	return nil
}

func writeManagedProxyInfo(info managedProxyInfo) error {
	if err := ensureProxyOutStateDir(); err != nil {
		return err
	}
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return os.WriteFile(proxyOutStatePath(info.ConnectionID), data, 0644)
}

func readManagedProxyInfo(connectionID string) (*managedProxyInfo, error) {
	data, err := os.ReadFile(proxyOutStatePath(connectionID))
	if err != nil {
		return nil, err
	}
	var info managedProxyInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func stopManagedProxy(connectionID string) error {
	info, err := readManagedProxyInfo(connectionID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.PID > 0 {
		process, err := os.FindProcess(info.PID)
		if err == nil {
			_ = process.Kill()
		}
	}
	if info.KubeconfigPath != "" {
		_ = os.Remove(info.KubeconfigPath)
	}
	_ = os.Remove(proxyOutStatePath(connectionID))
	return nil
}

func stopAllManagedProxies() error {
	entries, err := os.ReadDir(proxyOutStateDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		connectionID := entry.Name()[:len(entry.Name())-len(".json")]
		_ = stopManagedProxy(connectionID)
	}
	return nil
}
