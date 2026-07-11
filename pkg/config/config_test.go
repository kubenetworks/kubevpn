package config

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syncthing/syncthing/lib/protocol"
)

func TestGetSockPath_V2(t *testing.T) {
	userPath := GetSockPath(false)
	sudoPath := GetSockPath(true)

	if !strings.HasSuffix(userPath, SockPath) {
		t.Errorf("user sock path should end with %q, got %q", SockPath, userPath)
	}
	if !strings.HasSuffix(sudoPath, SudoSockPath) {
		t.Errorf("sudo sock path should end with %q, got %q", SudoSockPath, sudoPath)
	}
	if userPath == sudoPath {
		t.Error("user and sudo sock paths must differ")
	}
	// Both should be under the daemon directory
	if !strings.Contains(userPath, Daemon) {
		t.Errorf("user sock path should contain %q, got %q", Daemon, userPath)
	}
}

func TestGetPidPath_V2(t *testing.T) {
	userPath := GetPidPath(false)
	sudoPath := GetPidPath(true)

	if !strings.HasSuffix(userPath, PidPath) {
		t.Errorf("user pid path should end with %q, got %q", PidPath, userPath)
	}
	if !strings.HasSuffix(sudoPath, SudoPidPath) {
		t.Errorf("sudo pid path should end with %q, got %q", SudoPidPath, sudoPath)
	}
	if userPath == sudoPath {
		t.Error("user and sudo pid paths must differ")
	}
}

func TestGetDaemonLogPath(t *testing.T) {
	userLog := GetDaemonLogPath(false)
	sudoLog := GetDaemonLogPath(true)

	if !strings.HasSuffix(userLog, UserLogFile) {
		t.Errorf("user log path should end with %q, got %q", UserLogFile, userLog)
	}
	if !strings.HasSuffix(sudoLog, SudoLogFile) {
		t.Errorf("sudo log path should end with %q, got %q", SudoLogFile, sudoLog)
	}
	if userLog == sudoLog {
		t.Error("user and sudo log paths must differ")
	}
	if !strings.Contains(userLog, Log) {
		t.Errorf("log path should contain %q directory, got %q", Log, userLog)
	}
}

func TestGetDBPath_V2(t *testing.T) {
	dbPath := GetDBPath()
	if !strings.HasSuffix(dbPath, DBFile) {
		t.Errorf("db path should end with %q, got %q", DBFile, dbPath)
	}
	if !strings.Contains(dbPath, Daemon) {
		t.Errorf("db path should contain %q directory, got %q", Daemon, dbPath)
	}
}

func TestGetSyncthingPath(t *testing.T) {
	p := GetSyncthingPath()
	if !strings.HasSuffix(p, SyncthingDir) {
		t.Errorf("syncthing path should end with %q, got %q", SyncthingDir, p)
	}
	if !strings.Contains(p, Daemon) {
		t.Errorf("syncthing path should contain %q directory, got %q", Daemon, p)
	}
}

func TestGetConfigFile(t *testing.T) {
	p := GetConfigFile()
	if !strings.HasSuffix(p, ConfigFile) {
		t.Errorf("config file path should end with %q, got %q", ConfigFile, p)
	}
	if !strings.Contains(p, HOME) {
		t.Errorf("config file path should contain %q directory, got %q", HOME, p)
	}
}

func TestGetTempPath_V2(t *testing.T) {
	p := GetTempPath()
	if !strings.HasSuffix(p, TempDir) {
		t.Errorf("temp path should end with %q, got %q", TempDir, p)
	}
	if !strings.Contains(p, HOME) {
		t.Errorf("temp path should contain %q directory, got %q", HOME, p)
	}
}

func TestGetPProfPath(t *testing.T) {
	p := GetPProfPath()
	if !strings.HasSuffix(p, PProfDir) {
		t.Errorf("pprof path should end with %q, got %q", PProfDir, p)
	}
	if !strings.Contains(p, Daemon) {
		t.Errorf("pprof path should contain %q directory, got %q", Daemon, p)
	}
}

func TestPathsAreAbsolute(t *testing.T) {
	paths := map[string]string{
		"GetSockPath(false)":     GetSockPath(false),
		"GetSockPath(true)":      GetSockPath(true),
		"GetPidPath(false)":      GetPidPath(false),
		"GetPidPath(true)":       GetPidPath(true),
		"GetDaemonLogPath(false)": GetDaemonLogPath(false),
		"GetDaemonLogPath(true)":  GetDaemonLogPath(true),
		"GetDBPath()":            GetDBPath(),
		"GetSyncthingPath()":     GetSyncthingPath(),
		"GetConfigFile()":        GetConfigFile(),
		"GetTempPath()":          GetTempPath(),
		"GetPProfPath()":         GetPProfPath(),
	}
	for name, p := range paths {
		if !filepath.IsAbs(p) {
			t.Errorf("%s returned relative path: %q", name, p)
		}
	}
}

func TestPathsRootedInHomeDir(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("failed to get home dir: %v", err)
	}
	expected := filepath.Join(homeDir, HOME)

	paths := map[string]string{
		"GetSockPath":     GetSockPath(false),
		"GetPidPath":      GetPidPath(false),
		"GetDaemonLogPath": GetDaemonLogPath(false),
		"GetDBPath":       GetDBPath(),
		"GetSyncthingPath": GetSyncthingPath(),
		"GetConfigFile":   GetConfigFile(),
		"GetTempPath":     GetTempPath(),
		"GetPProfPath":    GetPProfPath(),
	}
	for name, p := range paths {
		if !strings.HasPrefix(p, expected) {
			t.Errorf("%s path %q should be under %q", name, p, expected)
		}
	}
}

func TestDirectoriesCreatedByInit(t *testing.T) {
	dirs := []string{
		GetSyncthingPath(),
		GetTempPath(),
		GetPProfPath(),
	}
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("directory %q should exist after init: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%q should be a directory", dir)
		}
	}
}

func TestConfigFileCreatedByInit(t *testing.T) {
	p := GetConfigFile()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("config file %q should exist after init: %v", p, err)
	}
	if info.IsDir() {
		t.Errorf("%q should be a file, not a directory", p)
	}
	if info.Size() == 0 {
		t.Error("config file should not be empty")
	}
}

// --- CIDR / network init tests ---

func TestIPv4PoolParsed(t *testing.T) {
	if CIDR == nil {
		t.Fatal("CIDR should be initialized")
	}
	if RouterIP == nil {
		t.Fatal("RouterIP should be initialized")
	}
	if !CIDR.Contains(RouterIP) {
		t.Errorf("RouterIP %v should be within CIDR %v", RouterIP, CIDR)
	}
	ones, bits := CIDR.Mask.Size()
	if ones != 16 || bits != 32 {
		t.Errorf("expected /16 IPv4 CIDR, got /%d (bits=%d)", ones, bits)
	}
}

func TestIPv6PoolParsed(t *testing.T) {
	if CIDR6 == nil {
		t.Fatal("CIDR6 should be initialized")
	}
	if RouterIP6 == nil {
		t.Fatal("RouterIP6 should be initialized")
	}
	if !CIDR6.Contains(RouterIP6) {
		t.Errorf("RouterIP6 %v should be within CIDR6 %v", RouterIP6, CIDR6)
	}
	ones, bits := CIDR6.Mask.Size()
	if ones != 64 || bits != 128 {
		t.Errorf("expected /64 IPv6 CIDR, got /%d (bits=%d)", ones, bits)
	}
}

func TestDockerCIDRParsed(t *testing.T) {
	if DockerCIDR == nil {
		t.Fatal("DockerCIDR should be initialized")
	}
	if DockerRouterIP == nil {
		t.Fatal("DockerRouterIP should be initialized")
	}
	if !DockerCIDR.Contains(DockerRouterIP) {
		t.Errorf("DockerRouterIP %v should be within DockerCIDR %v", DockerRouterIP, DockerCIDR)
	}
}

func TestIPv4AndDockerPoolsDoNotOverlap(t *testing.T) {
	// IPv4Pool = 198.18.0.0/16, DockerIPv4Pool = 198.19.0.1/16
	// They should not overlap
	dockerIP := net.ParseIP("198.19.0.1")
	if CIDR.Contains(dockerIP) {
		t.Error("Docker IP 198.19.0.1 should not be in the main IPv4 pool 198.18.0.0/16")
	}
	mainIP := net.ParseIP("198.18.0.1")
	if DockerCIDR.Contains(mainIP) {
		t.Error("Main IP 198.18.0.1 should not be in the Docker pool 198.19.0.0/16")
	}
}

// --- Constants validation ---

func TestDefaultMTU_V2(t *testing.T) {
	// MTU must be positive and less than standard Ethernet MTU
	if DefaultMTU <= 0 {
		t.Errorf("DefaultMTU should be positive, got %d", DefaultMTU)
	}
	if DefaultMTU >= 1500 {
		t.Errorf("DefaultMTU should be less than 1500 (Ethernet), got %d", DefaultMTU)
	}
	expected := 1500 - 40 - 20 - (5 + 1 + 16) - (2 + 1)
	if DefaultMTU != expected {
		t.Errorf("DefaultMTU = %d, expected %d", DefaultMTU, expected)
	}
}

func TestPProfPorts(t *testing.T) {
	if PProfPort == SudoPProfPort {
		t.Error("PProfPort and SudoPProfPort must differ")
	}
	if PProfPort <= 0 || SudoPProfPort <= 0 {
		t.Error("pprof ports must be positive")
	}
}

func TestTimeoutValues(t *testing.T) {
	if KeepAliveTime <= 0 {
		t.Error("KeepAliveTime must be positive")
	}
	if DialTimeout <= 0 {
		t.Error("DialTimeout must be positive")
	}
	if ConnectTimeout <= 0 {
		t.Error("ConnectTimeout must be positive")
	}
	if UDPSessionTimeout <= 0 {
		t.Error("UDPSessionTimeout must be positive")
	}
}

// --- Syncthing tests ---

func TestSyncthingDeviceIDs(t *testing.T) {
	// Verify the device IDs are valid by parsing them
	local, err := protocol.DeviceIDFromString(SyncthingLocalDeviceID)
	if err != nil {
		t.Fatalf("failed to parse local device ID: %v", err)
	}
	remote, err := protocol.DeviceIDFromString(SyncthingRemoteDeviceID)
	if err != nil {
		t.Fatalf("failed to parse remote device ID: %v", err)
	}

	// Verify they match the init-parsed values
	if local != LocalDeviceID {
		t.Errorf("parsed local device ID %v != init-parsed %v", local, LocalDeviceID)
	}
	if remote != RemoteDeviceID {
		t.Errorf("parsed remote device ID %v != init-parsed %v", remote, RemoteDeviceID)
	}
	if LocalDeviceID == RemoteDeviceID {
		t.Error("local and remote device IDs must differ")
	}
}

func TestSyncthingCertificates(t *testing.T) {
	if len(LocalCert.Certificate) == 0 {
		t.Error("LocalCert should have at least one certificate")
	}
	if len(RemoteCert.Certificate) == 0 {
		t.Error("RemoteCert should have at least one certificate")
	}
	if LocalCert.PrivateKey == nil {
		t.Error("LocalCert private key should not be nil")
	}
	if RemoteCert.PrivateKey == nil {
		t.Error("RemoteCert private key should not be nil")
	}
}

// --- Image / version ---

func TestImageNotEmpty(t *testing.T) {
	if Image == "" {
		t.Error("Image should not be empty")
	}
	if Version == "" {
		t.Error("Version should not be empty")
	}
}

func TestImageContainsRegistry(t *testing.T) {
	if !strings.Contains(Image, "ghcr.io") && !strings.Contains(Image, "docker.io") {
		// Image can be overridden via ldflags, but the default should have a registry
		t.Logf("Image %q does not contain a known registry prefix (may be overridden via ldflags)", Image)
	}
}

// --- ConfigMap / label / env constants ---

func TestConfigMapAndKeyConstants(t *testing.T) {
	constants := map[string]string{
		"ConfigMapPodTrafficManager": ConfigMapPodTrafficManager,
		"KeyDHCP":                    KeyDHCP,
		"KeyDHCP6":                   KeyDHCP6,
		"KeyEnvoy":                   KeyEnvoy,
		"KeyClusterIPv4POOLS":        KeyClusterIPv4POOLS,
	}
	for name, val := range constants {
		if val == "" {
			t.Errorf("%s should not be empty", name)
		}
	}
}

func TestContainerNameConstants(t *testing.T) {
	names := []string{
		ContainerSidecarEnvoyProxy,
		ContainerSidecarControlPlane,
		ContainerSidecarDNS,
		ContainerSidecarVPN,
		ContainerSidecarSyncthing,
	}
	seen := make(map[string]bool)
	for _, name := range names {
		if name == "" {
			t.Error("container sidecar name should not be empty")
		}
		if seen[name] {
			t.Errorf("duplicate container name: %q", name)
		}
		seen[name] = true
	}
}

func TestTLSConstants(t *testing.T) {
	if TLSCertKey == "" {
		t.Error("TLSCertKey should not be empty")
	}
	if TLSPrivateKeyKey == "" {
		t.Error("TLSPrivateKeyKey should not be empty")
	}
	if TLSServerName == "" {
		t.Error("TLSServerName should not be empty")
	}
}

func TestEnvNameConstants(t *testing.T) {
	envs := map[string]string{
		"EnvInboundPodTunIPv4": EnvInboundPodTunIPv4,
		"EnvInboundPodTunIPv6": EnvInboundPodTunIPv6,
		"EnvPodName":           EnvPodName,
		"EnvPodNamespace":      EnvPodNamespace,
		"EnvSSHJump":           EnvSSHJump,
	}
	for name, val := range envs {
		if val == "" {
			t.Errorf("%s should not be empty", name)
		}
	}
}

func TestHostsKeyword(t *testing.T) {
	if HostsKeyword == "" {
		t.Error("HostsKeyword should not be empty")
	}
	if !strings.Contains(HostsDeviceKeyword, HostsKeyword) {
		t.Errorf("HostsDeviceKeyword %q should contain HostsKeyword %q", HostsDeviceKeyword, HostsKeyword)
	}
	if !strings.Contains(HostsDeviceKeyword, "%s") {
		t.Error("HostsDeviceKeyword should contain a format verb for the device name")
	}
}
