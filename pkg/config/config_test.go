package config

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestConstants(t *testing.T) {
	if ConfigMapPodTrafficManager == "" {
		t.Fatal("ConfigMapPodTrafficManager is empty")
	}
	if DefaultMTU <= 0 || DefaultMTU > 1500 {
		t.Fatalf("DefaultMTU out of range: %d", DefaultMTU)
	}
	if CIDR == nil {
		t.Fatal("CIDR is nil")
	}
	if CIDR6 == nil {
		t.Fatal("CIDR6 is nil")
	}
}

func TestGetDBPath(t *testing.T) {
	path := GetDBPath()
	if path == "" {
		t.Fatal("GetDBPath() returned empty string")
	}
	if !strings.Contains(path, HOME) {
		t.Fatalf("GetDBPath() = %q, expected to contain %q", path, HOME)
	}
	if !strings.HasSuffix(path, DBFile) {
		t.Fatalf("GetDBPath() = %q, expected to end with %q", path, DBFile)
	}
}

func TestGetSockPath(t *testing.T) {
	userPath := GetSockPath(false)
	sudoPath := GetSockPath(true)

	if userPath == "" {
		t.Fatal("GetSockPath(false) returned empty string")
	}
	if sudoPath == "" {
		t.Fatal("GetSockPath(true) returned empty string")
	}
	if userPath == sudoPath {
		t.Fatalf("user and sudo sock paths should differ: %q == %q", userPath, sudoPath)
	}
	if !strings.HasSuffix(userPath, SockPath) {
		t.Fatalf("GetSockPath(false) = %q, expected suffix %q", userPath, SockPath)
	}
	if !strings.HasSuffix(sudoPath, SudoSockPath) {
		t.Fatalf("GetSockPath(true) = %q, expected suffix %q", sudoPath, SudoSockPath)
	}
}

func TestGetPidPath(t *testing.T) {
	userPath := GetPidPath(false)
	sudoPath := GetPidPath(true)

	if userPath == "" {
		t.Fatal("GetPidPath(false) returned empty string")
	}
	if sudoPath == "" {
		t.Fatal("GetPidPath(true) returned empty string")
	}
	if userPath == sudoPath {
		t.Fatalf("user and sudo pid paths should differ: %q == %q", userPath, sudoPath)
	}
	if !strings.HasSuffix(userPath, PidPath) {
		t.Fatalf("GetPidPath(false) = %q, expected suffix %q", userPath, PidPath)
	}
	if !strings.HasSuffix(sudoPath, SudoPidPath) {
		t.Fatalf("GetPidPath(true) = %q, expected suffix %q", sudoPath, SudoPidPath)
	}
}

func TestGetTempPath(t *testing.T) {
	path := GetTempPath()
	if path == "" {
		t.Fatal("GetTempPath() returned empty string")
	}
	if !strings.Contains(path, HOME) {
		t.Fatalf("GetTempPath() = %q, expected to contain %q", path, HOME)
	}
	if !strings.HasSuffix(path, TempDir) {
		t.Fatalf("GetTempPath() = %q, expected to end with %q", path, TempDir)
	}
}

func TestInitSetsValidCIDR(t *testing.T) {
	// CIDR should be the parsed form of IPv4Pool
	if CIDR == nil {
		t.Fatal("CIDR is nil after init")
	}
	ones, bits := CIDR.Mask.Size()
	if bits != 32 {
		t.Fatalf("CIDR mask bits = %d, want 32", bits)
	}
	if ones != 16 {
		t.Fatalf("CIDR mask ones = %d, want 16", ones)
	}
	// Verify it contains the expected network
	expectedIP := net.ParseIP("198.18.0.0")
	if !CIDR.IP.Equal(expectedIP) {
		t.Fatalf("CIDR.IP = %v, want %v", CIDR.IP, expectedIP)
	}
}

func TestInitSetsValidCIDR6(t *testing.T) {
	if CIDR6 == nil {
		t.Fatal("CIDR6 is nil after init")
	}
	ones, bits := CIDR6.Mask.Size()
	if bits != 128 {
		t.Fatalf("CIDR6 mask bits = %d, want 128", bits)
	}
	if ones != 64 {
		t.Fatalf("CIDR6 mask ones = %d, want 64", ones)
	}
}

func TestInitSetsRouterIPs(t *testing.T) {
	if RouterIP == nil {
		t.Fatal("RouterIP is nil after init")
	}
	if RouterIP6 == nil {
		t.Fatal("RouterIP6 is nil after init")
	}
	// RouterIP should be within the CIDR
	if !CIDR.Contains(RouterIP) {
		t.Fatalf("RouterIP %v is not within CIDR %v", RouterIP, CIDR)
	}
	if !CIDR6.Contains(RouterIP6) {
		t.Fatalf("RouterIP6 %v is not within CIDR6 %v", RouterIP6, CIDR6)
	}
}

func TestInitSetsDockerCIDR(t *testing.T) {
	if DockerCIDR == nil {
		t.Fatal("DockerCIDR is nil after init")
	}
	if DockerRouterIP == nil {
		t.Fatal("DockerRouterIP is nil after init")
	}
	ones, bits := DockerCIDR.Mask.Size()
	if bits != 32 {
		t.Fatalf("DockerCIDR mask bits = %d, want 32", bits)
	}
	if ones != 16 {
		t.Fatalf("DockerCIDR mask ones = %d, want 16", ones)
	}
}

func TestPoolAllocations(t *testing.T) {
	t.Run("LPool returns LargeBufferSize", func(t *testing.T) {
		buf := LPool.Get().([]byte)
		defer LPool.Put(buf)
		if len(buf) != LargeBufferSize {
			t.Fatalf("LPool buffer len = %d, want %d", len(buf), LargeBufferSize)
		}
		if cap(buf) < LargeBufferSize {
			t.Fatalf("LPool buffer cap = %d, want >= %d", cap(buf), LargeBufferSize)
		}
	})

	t.Run("LPool buffer is 64KB", func(t *testing.T) {
		buf := LPool.Get().([]byte)
		defer LPool.Put(buf)
		expected := 64 * 1024
		if len(buf) != expected {
			t.Fatalf("LPool buffer len = %d, want %d (64KB)", len(buf), expected)
		}
	})
}

func TestTimingConstants(t *testing.T) {
	cases := []struct {
		name string
		val  time.Duration
		min  time.Duration
		max  time.Duration
	}{
		{"KeepAliveTime", KeepAliveTime, 30 * time.Second, 120 * time.Second},
		{"DialTimeout", DialTimeout, 5 * time.Second, 30 * time.Second},
		{"HealthCheckInterval", HealthCheckInterval, 10 * time.Second, 60 * time.Second},
		{"SSHKeepAliveInterval", SSHKeepAliveInterval, 5 * time.Second, 30 * time.Second},
		{"UDPSessionTimeout", UDPSessionTimeout, 30 * time.Second, 300 * time.Second},
		{"UDPRelayTimeout", UDPRelayTimeout, 10 * time.Second, 60 * time.Second},
		{"SlotReconnectBackoff", SlotReconnectBackoff, 1 * time.Second, 10 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.val < c.min || c.val > c.max {
				t.Fatalf("%s = %v, want [%v, %v]", c.name, c.val, c.min, c.max)
			}
		})
	}
}

func TestPortNameConstants(t *testing.T) {
	if PortNameTCP == "" || PortNameEnvoy == "" || PortNameHTTP == "" || PortNameDNS == "" {
		t.Fatal("port name constants must not be empty")
	}
}

func TestErrors(t *testing.T) {
	errs := []error{
		ErrInvalidKubeconfig,
		ErrPortForwardTimeout,
	}
	for _, err := range errs {
		if err == nil {
			t.Fatal("nil sentinel error")
		}
		if err.Error() == "" {
			t.Fatal("empty error message")
		}
	}
}

func TestBufferSizes(t *testing.T) {
	if LargeBufferSize <= 0 {
		t.Fatal("LargeBufferSize <= 0")
	}
	expected := 64 * 1024
	if LargeBufferSize != expected {
		t.Fatalf("LargeBufferSize = %d, want %d", LargeBufferSize, expected)
	}
}

func TestDefaultMTU(t *testing.T) {
	// mtu = 1500 - max(20,40) - 20 - (5+1+16) - (2+1) = 1500 - 40 - 20 - 22 - 3 = 1415
	const expectedMTU = 1415
	if DefaultMTU != expectedMTU {
		t.Fatalf("DefaultMTU = %d, want %d", DefaultMTU, expectedMTU)
	}
}

func TestIPv4Pool_ParsesCIDR(t *testing.T) {
	_, expected, err := net.ParseCIDR(IPv4Pool)
	if err != nil {
		t.Fatalf("failed to parse IPv4Pool %q: %v", IPv4Pool, err)
	}
	// Verify the parsed CIDR matches what config.CIDR holds
	if !CIDR.IP.Equal(expected.IP) {
		t.Errorf("CIDR.IP = %v, want %v", CIDR.IP, expected.IP)
	}
	if CIDR.Mask.String() != expected.Mask.String() {
		t.Errorf("CIDR.Mask = %v, want %v", CIDR.Mask, expected.Mask)
	}
	// Verify expected network is 198.18.0.0/16
	if expected.IP.String() != "198.18.0.0" {
		t.Errorf("expected IP 198.18.0.0, got %s", expected.IP.String())
	}
	ones, bits := expected.Mask.Size()
	if ones != 16 || bits != 32 {
		t.Errorf("expected /16 mask, got /%d (bits=%d)", ones, bits)
	}
}
