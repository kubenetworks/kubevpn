package util

import (
	"strings"
	"testing"
)

func TestIsNewer(t *testing.T) {
	type args struct {
		clientVersionStr string
		clientImgStr     string
		serverImgStr     string
	}
	tests := []struct {
		name    string
		args    args
		want    bool
		wantErr bool
	}{
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: naison/kubevpn:v1.0.0
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) newer than server(naison/kubevpn:v1.0.0)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "naison/kubevpn:v1.0.0",
			},
			want:    true,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: docker.io/naison/kubevpn:v1.0.0
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) newer than server(docker.io/naison/kubevpn:v1.0.0)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "docker.io/naison/kubevpn:v1.0.0",
			},
			want:    true,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: naison/kubevpn:v1.2.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) same as server(naison/kubevpn:v1.2.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "naison/kubevpn:v1.2.1",
			},
			want:    false,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: docker.io/naison/kubevpn:v1.2.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) same as server(docker.io/naison/kubevpn:v1.2.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "docker.io/naison/kubevpn:v1.2.1",
			},
			want:    false,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: docker.io/naison/kubevpn:v1.3.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) older as server(docker.io/naison/kubevpn:v1.3.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "docker.io/naison/kubevpn:v1.3.1",
			},
			want:    true,
			wantErr: true,
		},
		// client version: v1.3.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1 (not same as client version, --image=xxx)
		// server image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		{
			name: "Valid case - client cli version(v1.3.1) not same as client image(ghcr.io/kubenetworks/kubevpn:v1.2.1)",
			args: args{
				clientVersionStr: "v1.3.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
			},
			want:    true,
			wantErr: true,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: ghcr.io/kubenetworks/kubevpn:v1.0.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) newer than server(ghcr.io/kubenetworks/kubevpn:v1.0.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.0.1",
			},
			want:    true,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) same as server(ghcr.io/kubenetworks/kubevpn:v1.2.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
			},
			want:    false,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: ghcr.io/kubenetworks/kubevpn:v1.3.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) older as server(ghcr.io/kubenetworks/kubevpn:v1.3.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.3.1",
			},
			want:    true,
			wantErr: true,
		},

		// custom server image registry, but client image is not same as client version, does not upgrade
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: mykubevpn.io/kubenetworks/kubevpn:v1.1.1
		{
			name: "custom server image registry, but client image is not same as client version, does not upgrade",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "mykubevpn.io/kubenetworks/kubevpn:v1.1.1",
			},
			want:    true,
			wantErr: false,
		},

		// custom server image registry, client image is same as client version,  upgrade
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: mykubevpn.io/kubenetworks/kubevpn:v1.1.1
		{
			name: "custom server image registry, client image is same as client version,  upgrade",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "mykubevpn.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "mykubevpn.io/kubenetworks/kubevpn:v1.1.1",
			},
			want:    true,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IsNewer(tt.args.clientVersionStr, tt.args.clientImgStr, tt.args.serverImgStr)
			if (err != nil) != tt.wantErr {
				t.Errorf("newer() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("newer() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetTargetImage(t *testing.T) {
	type args struct {
		version string
		image   string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "replace version",
			args: args{
				version: "v1.2.3",
				image:   "ghcr.io/kubenetworks/kubevpn:v1.0.0",
			},
			want: "ghcr.io/kubenetworks/kubevpn:v1.2.3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetTargetImage(tt.args.version, tt.args.image); got != tt.want {
				t.Errorf("GetTargetImage() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCmpClientVersionAndPodImageTag(t *testing.T) {
	type args struct {
		clientVersionStr string
		serverImgStr     string
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "Major version is diff",
			args: args{
				clientVersionStr: "v2.2.3",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.0",
			},
			want: true,
		},
		{
			name: "Minor version is diff",
			args: args{
				clientVersionStr: "v1.2.3",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.0.0",
			},
			want: true,
		},
		{
			name: "PATCH version is diff",
			args: args{
				clientVersionStr: "v2.2.3",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v2.2.0",
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CmpClientVersionAndPodImageTag(tt.args.clientVersionStr, tt.args.serverImgStr); (got != 0) != tt.want {
				t.Errorf("CmpClientVersionAndPodImageTag() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFormatBanner(t *testing.T) {
	banner := FormatBanner("hello world")
	if !strings.Contains(banner, "hello world") {
		t.Fatalf("FormatBanner output should contain the message, got: %s", banner)
	}
	if !strings.HasPrefix(banner, "+") {
		t.Fatalf("FormatBanner output should start with '+', got: %s", banner)
	}
	if !strings.HasSuffix(banner, "+") {
		t.Fatalf("FormatBanner output should end with '+', got: %s", banner)
	}
	// Verify multi-line input works
	multiLine := FormatBanner("line one\nline two")
	if !strings.Contains(multiLine, "line one") || !strings.Contains(multiLine, "line two") {
		t.Fatalf("FormatBanner should contain both lines, got: %s", multiLine)
	}
}

func TestCheckVersionCompatibility_SameVersion(t *testing.T) {
	// Same MAJOR.MINOR should return 0 (compatible)
	result := CmpVersionMajorOrMinor("v2.3.1", "v2.3.5")
	if result != 0 {
		t.Fatalf("expected 0 for compatible versions (same major.minor), got %d", result)
	}
}

func TestCheckVersionCompatibility_IncompatibleMajor(t *testing.T) {
	// Different MAJOR should return non-zero (incompatible)
	result := CmpVersionMajorOrMinor("v2.3.1", "v1.3.1")
	if result == 0 {
		t.Fatal("expected non-zero for incompatible versions (different major)")
	}
	if result != 1 {
		t.Fatalf("expected 1 when v1 major > v2 major, got %d", result)
	}
}

func TestCheckVersionCompatibility_IncompatibleMinor(t *testing.T) {
	// Different MINOR should return non-zero (incompatible)
	result := CmpVersionMajorOrMinor("v2.3.1", "v2.1.9")
	if result == 0 {
		t.Fatal("expected non-zero for incompatible versions (different minor)")
	}
	if result != 1 {
		t.Fatalf("expected 1 when v1 minor > v2 minor, got %d", result)
	}
}

func TestCheckVersionCompatibility_OlderVersion(t *testing.T) {
	// v1 older than v2 should return -1
	result := CmpVersionMajorOrMinor("v1.2.0", "v1.5.0")
	if result != -1 {
		t.Fatalf("expected -1 when v1 minor < v2 minor, got %d", result)
	}
}

func TestCheckVersionCompatibility_InvalidVersion(t *testing.T) {
	// Invalid version string should return 0 (no comparison possible)
	result := CmpVersionMajorOrMinor("not-a-version", "v1.2.3")
	if result != 0 {
		t.Fatalf("expected 0 for invalid version input, got %d", result)
	}
}

// TestCmpVersionMajorOrMinor_SHANotComparable is a regression for the CI failure where a git-SHA
// client version and a SHA-tagged image were wrongly judged incompatible (the lenient parser read
// "89b23e73..." as major 89). Strict semver parsing must treat SHAs as non-comparable (0).
func TestCmpVersionMajorOrMinor_SHANotComparable(t *testing.T) {
	cases := [][2]string{
		{"378749d", "89b23e730b0c9d0b91640eb52bf3da33e81d3415"}, // both SHA
		{"378749d", "v2.10.2"},                                  // SHA vs real semver
		{"v2.10.2", "89b23e730b0c9d0b91640eb52bf3da33e81d3415"}, // real semver vs SHA
		{"latest", "v2.10.2"},                                   // "latest" default
	}
	for _, c := range cases {
		if got := CmpVersionMajorOrMinor(c[0], c[1]); got != 0 {
			t.Errorf("CmpVersionMajorOrMinor(%q, %q) = %d, want 0 (non-comparable)", c[0], c[1], got)
		}
	}
}

// TestCmpClientVersionAndClientImage_SHACompatible reproduces the exact sidecar-crash inputs:
// the version gate must NOT reject a SHA client version against a SHA-tagged image.
func TestCmpClientVersionAndClientImage_SHACompatible(t *testing.T) {
	cmp, err := CmpClientVersionAndClientImage("378749d", "ghcr.io/kubenetworks/kubevpn:89b23e730b0c9d0b91640eb52bf3da33e81d3415")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmp != 0 {
		t.Fatalf("expected compatible (0) for SHA client vs SHA image tag, got %d", cmp)
	}
}

// TestIsNewer_SHACompatible asserts the upgrade gate no longer errors for SHA client/image,
// which previously crash-looped the injected sidecar (UpgradeDeploy).
func TestIsNewer_SHACompatible(t *testing.T) {
	img := "ghcr.io/kubenetworks/kubevpn:89b23e730b0c9d0b91640eb52bf3da33e81d3415"
	needUpgrade, err := IsNewer("378749d", img, img)
	if err != nil {
		t.Fatalf("IsNewer must not error for SHA client/image, got: %v", err)
	}
	if needUpgrade {
		t.Fatalf("expected no upgrade needed for SHA client/image, got true")
	}
}

// TestCmpVersionMajorOrMinor_RealSemverStillCompared guards that strict parsing keeps real
// release comparisons working.
func TestCmpVersionMajorOrMinor_RealSemverStillCompared(t *testing.T) {
	if CmpVersionMajorOrMinor("v2.11.0", "v2.10.5") <= 0 {
		t.Error("v2.11.0 should be newer than v2.10.5")
	}
	if CmpVersionMajorOrMinor("v2.10.0", "v2.11.0") >= 0 {
		t.Error("v2.10.0 should be older than v2.11.0")
	}
	if CmpVersionMajorOrMinor("v2.10.1", "v2.10.9") != 0 {
		t.Error("same major.minor should compare equal (patch ignored)")
	}
}
