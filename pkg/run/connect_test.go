package run

import (
	"fmt"
	"slices"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

func containsFlagValue(entrypoint []string, flag, value string) bool {
	for i := 0; i < len(entrypoint)-1; i++ {
		if entrypoint[i] == flag && entrypoint[i+1] == value {
			return true
		}
	}
	return false
}

func TestBuildConnectEntrypoint(t *testing.T) {
	tests := []struct {
		name             string
		option           Options
		managerNamespace string
		wantPrefix       []string
		wantContains     [][2]string // flag-value pairs
		wantHasFlags     []string    // standalone flags (boolean)
		wantNotContains  []string    // flags that must be absent
	}{
		{
			name: "connect mode with empty managerNamespace",
			option: Options{
				NoProxy:   true,
				Namespace: "default",
			},
			managerNamespace: "",
			wantPrefix:       []string{"kubevpn", "connect"},
			wantContains: [][2]string{
				{"--image", config.Image},
				{"--kubeconfig", "/root/.kube/config"},
				{"-n", "default"},
			},
			wantHasFlags:    []string{"--foreground"},
			wantNotContains: []string{"--manager-namespace"},
		},
		{
			name: "proxy mode with custom managerNamespace",
			option: Options{
				NoProxy:   false,
				Namespace: "test-ns",
				Workload:  "deployments/web",
			},
			managerNamespace: "kubevpn-system",
			wantPrefix:       []string{"kubevpn", "proxy", "deployments/web"},
			wantContains: [][2]string{
				{"--image", config.Image},
				{"--kubeconfig", "/root/.kube/config"},
				{"-n", "test-ns"},
				{"--manager-namespace", "kubevpn-system"},
			},
			wantHasFlags: []string{"--foreground"},
		},
		{
			name: "proxy mode with empty managerNamespace passes empty string",
			option: Options{
				NoProxy:   false,
				Namespace: "dev",
				Workload:  "statefulsets/db",
			},
			managerNamespace: "",
			wantPrefix:       []string{"kubevpn", "proxy", "statefulsets/db"},
			wantContains: [][2]string{
				{"--manager-namespace", ""},
			},
		},
		{
			name: "connect mode does not include headers",
			option: Options{
				NoProxy:   true,
				Namespace: "default",
				Headers:   map[string]string{"x-env": "dev"},
			},
			managerNamespace: "",
			wantPrefix:       []string{"kubevpn", "connect"},
			wantNotContains:  []string{"--headers"},
		},
		{
			name: "proxy mode includes headers",
			option: Options{
				NoProxy:   false,
				Namespace: "default",
				Workload:  "deployments/api",
				Headers:   map[string]string{"x-env": "dev"},
			},
			managerNamespace: "mgr",
			wantContains: [][2]string{
				{"--headers", "x-env=dev"},
			},
		},
		{
			name: "extra CIDR and domain appended",
			option: Options{
				NoProxy:   true,
				Namespace: "default",
				ExtraRouteInfo: handler.ExtraRouteInfo{
					ExtraCIDR:   []string{"10.0.0.0/8", "172.16.0.0/12"},
					ExtraDomain: []string{"example.com"},
				},
			},
			managerNamespace: "",
			wantContains: [][2]string{
				{"--extra-cidr", "10.0.0.0/8"},
				{"--extra-cidr", "172.16.0.0/12"},
				{"--extra-domain", "example.com"},
			},
		},
		{
			name: "extra node IP flag present when true",
			option: Options{
				NoProxy:   true,
				Namespace: "default",
				ExtraRouteInfo: handler.ExtraRouteInfo{
					ExtraNodeIP: true,
				},
			},
			managerNamespace: "",
			wantHasFlags:     []string{"--extra-node-ip"},
		},
		{
			name: "extra node IP flag absent when false",
			option: Options{
				NoProxy:   true,
				Namespace: "default",
				ExtraRouteInfo: handler.ExtraRouteInfo{
					ExtraNodeIP: false,
				},
			},
			managerNamespace: "",
			wantNotContains:  []string{"--extra-node-ip"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.option.buildConnectEntrypoint(tt.managerNamespace)

			// Check prefix
			if len(tt.wantPrefix) > 0 {
				if len(got) < len(tt.wantPrefix) {
					t.Fatalf("entrypoint too short: got %v, want prefix %v", got, tt.wantPrefix)
				}
				for i, want := range tt.wantPrefix {
					if got[i] != want {
						t.Errorf("entrypoint[%d] = %q, want %q (full: %v)", i, got[i], want, got)
					}
				}
			}

			// Check flag-value pairs
			for _, pair := range tt.wantContains {
				if !containsFlagValue(got, pair[0], pair[1]) {
					t.Errorf("entrypoint missing %s %s (got: %v)", pair[0], pair[1], got)
				}
			}

			// Check standalone flags
			for _, flag := range tt.wantHasFlags {
				if !slices.Contains(got, flag) {
					t.Errorf("entrypoint missing flag %s (got: %v)", flag, got)
				}
			}

			// Check absent flags
			for _, flag := range tt.wantNotContains {
				if slices.Contains(got, flag) {
					t.Errorf("entrypoint should not contain %s (got: %v)", flag, got)
				}
			}
		})
	}
}

func TestBuildConnectEntrypointRequiredFlags(t *testing.T) {
	option := Options{
		NoProxy:   true,
		Namespace: "my-ns",
	}
	got := option.buildConnectEntrypoint("")

	requiredFlags := []string{"--image", "--kubeconfig", "-n"}
	for _, flag := range requiredFlags {
		if !slices.Contains(got, flag) {
			t.Errorf("entrypoint missing required flag %s (got: %v)", flag, got)
		}
	}
}

func TestBuildConnectEntrypointImageMatchesConfig(t *testing.T) {
	origImage := config.Image
	config.Image = "custom-registry.io/kubevpn:v1.2.3"
	defer func() { config.Image = origImage }()

	option := Options{
		NoProxy:   true,
		Namespace: "default",
	}
	got := option.buildConnectEntrypoint("")

	if !containsFlagValue(got, "--image", "custom-registry.io/kubevpn:v1.2.3") {
		t.Errorf("--image should match config.Image, got: %v", got)
	}
}

func TestBuildConnectEntrypointMultipleHeaders(t *testing.T) {
	option := Options{
		NoProxy:   false,
		Namespace: "default",
		Workload:  "deployments/svc",
		Headers: map[string]string{
			"x-user":    "alice",
			"x-version": "v2",
		},
	}
	got := option.buildConnectEntrypoint("ns")

	headerCount := 0
	headerValues := make(map[string]bool)
	for i, v := range got {
		if v == "--headers" && i+1 < len(got) {
			headerCount++
			headerValues[got[i+1]] = true
		}
	}
	if headerCount != 2 {
		t.Errorf("expected 2 --headers flags, got %d (entrypoint: %v)", headerCount, got)
	}
	for _, expected := range []string{"x-user=alice", "x-version=v2"} {
		if !headerValues[expected] {
			t.Errorf("missing header value %q (got values: %v)", expected, headerValues)
		}
	}
}

func TestBuildConnectEntrypointProxyWorkloads(t *testing.T) {
	workloads := []string{
		"deployments/nginx",
		"statefulsets/redis",
		"pods/my-pod",
	}
	for _, workload := range workloads {
		t.Run(workload, func(t *testing.T) {
			option := Options{
				NoProxy:   false,
				Namespace: "default",
				Workload:  workload,
			}
			got := option.buildConnectEntrypoint("ns")

			if len(got) < 3 {
				t.Fatalf("entrypoint too short: %v", got)
			}
			if got[0] != "kubevpn" || got[1] != "proxy" || got[2] != workload {
				t.Errorf("entrypoint prefix = %v, want [kubevpn proxy %s]", got[:3], workload)
			}
		})
	}
}

// Ensure the fmt import is used.
var _ = fmt.Sprint
