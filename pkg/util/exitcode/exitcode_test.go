package exitcode

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TestClassify covers the sentinel/apierrors classification used both server-side
// (AsStatusError) and client-side (FromError for local errors).
func TestClassify(t *testing.T) {
	gr := schema.GroupResource{Group: "apps", Resource: "deployments"}
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, Success},
		{"unclassified", errors.New("boom"), Generic},

		{"kubeconfig invalid", fmt.Errorf("x: %w", config.ErrInvalidKubeconfig), KubeconfigInvalid},
		{"kubeconfig wrong protocol", fmt.Errorf("x: %w", config.ErrKubeconfigWrongProtocol), KubeconfigWrongProtocol},
		{"kubeconfig unresolvable", fmt.Errorf("x: %w", config.ErrKubeconfigUnresolvable), KubeconfigUnresolvable},

		{"permission sentinel", fmt.Errorf("x: %w", config.ErrPermissionDenied), PermissionDenied},

		{"port-forward timeout", fmt.Errorf("x: %w", config.ErrPortForwardTimeout), PortForwardTimeout},
		{"control plane not serving", fmt.Errorf("x: %w", config.ErrControlPlaneNotServing), ControlPlaneNotServing},

		{"connection not found", fmt.Errorf("x: %w", config.ErrConnectionNotFound), ConnectionNotFound},
		{"resource not found", fmt.Errorf("x: %w", config.ErrNotFound), NotFound},

		{"tun device", fmt.Errorf("x: %w", config.ErrTunDeviceFailed), TunDeviceFailed},
		{"route setup", fmt.Errorf("x: %w", config.ErrRouteSetupFailed), RouteSetupFailed},
		{"dns setup", fmt.Errorf("x: %w", config.ErrDNSSetupFailed), DNSSetupFailed},
		{"dhcp exhausted", fmt.Errorf("x: %w", config.ErrDHCPExhausted), DHCPExhausted},
		{"tun ip conflict", fmt.Errorf("x: %w", config.ErrTunIPConflict), TunIPConflict},

		{"tm deploy", fmt.Errorf("x: %w", config.ErrTrafficManagerDeploy), TrafficManagerDeployFailed},
		{"tm timeout", fmt.Errorf("x: %w", config.ErrTrafficManagerTimeout), TrafficManagerTimeout},
		{"image pull", fmt.Errorf("x: %w", config.ErrImagePull), ImagePullFailed},
		{"envoy inject", fmt.Errorf("x: %w", config.ErrEnvoyInject), EnvoyInjectFailed},

		{"daemon not running", fmt.Errorf("x: %w", config.ErrDaemonNotRunning), DaemonNotRunning},
		{"daemon version mismatch", fmt.Errorf("x: %w", config.ErrDaemonVersionMismatch), DaemonVersionMismatch},

		{"cleanup failed", fmt.Errorf("x: %w", config.ErrCleanupFailed), CleanupFailed},
		{"invalid argument", fmt.Errorf("x: %w", config.ErrInvalidArgument), InvalidArgument},

		{"ssh connect", fmt.Errorf("x: %w", config.ErrSSHConnect), SSHConnect},
		{"ssh auth", fmt.Errorf("x: %w", config.ErrSSHAuth), SSHAuth},
		{"ssh config", fmt.Errorf("x: %w", config.ErrSSHConfig), SSHConfig},
		{"gssapi", fmt.Errorf("x: %w", config.ErrGSSAPI), GSSAPI},
		{"ssh remote command", fmt.Errorf("x: %w", config.ErrSSHRemoteCommand), SSHRemoteCommand},

		{"syncthing", fmt.Errorf("x: %w", config.ErrSyncthing), SyncthingFailed},

		{"docker daemon", fmt.Errorf("x: %w", config.ErrDockerDaemonNotRunning), DockerDaemonNotRunning},
		{"docker image pull", fmt.Errorf("x: %w", config.ErrDockerImagePull), DockerImagePull},
		{"docker run", fmt.Errorf("x: %w", config.ErrDockerRun), DockerRunFailed},

		{"upgrade network", fmt.Errorf("x: %w", config.ErrUpgradeNetwork), UpgradeNetworkFailed},
		{"upgrade platform", fmt.Errorf("x: %w", config.ErrUpgradeUnsupportedPlatform), UpgradeUnsupportedPlatform},
		{"upgrade install", fmt.Errorf("x: %w", config.ErrUpgradeInstall), UpgradeInstallFailed},

		{"context canceled", context.Canceled, Interrupted},
		{"context deadline", context.DeadlineExceeded, Timeout},

		// Kubernetes apierrors auto-classification (unwrap %w).
		{"k8s forbidden", fmt.Errorf("create: %w", apierrors.NewForbidden(gr, "d", errors.New("no"))), PermissionDenied},
		{"k8s unauthorized", apierrors.NewUnauthorized("bad token"), Unauthenticated},
		{"k8s notfound", fmt.Errorf("get: %w", apierrors.NewNotFound(gr, "d")), NotFound},
		{"k8s alreadyexists", apierrors.NewAlreadyExists(gr, "d"), AlreadyExists},
		{"k8s timeout", apierrors.NewTimeoutError("slow", 1), Timeout},

		// Security wins over a broad traffic-manager wrap.
		{"forbidden under tm wrap", fmt.Errorf("%w: %w", apierrors.NewForbidden(gr, "d", errors.New("no")), config.ErrTrafficManagerDeploy), PermissionDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _, _ := Classify(tt.err); got != tt.want {
				t.Fatalf("Classify(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// TestFromError_RoundTripThroughStatus verifies the rich code survives encoding into
// a gRPC status detail and being read back (the daemon→CLI path).
func TestFromError_RoundTripThroughStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, Success},
		{"dhcp exhausted", fmt.Errorf("x: %w", config.ErrDHCPExhausted), DHCPExhausted},
		{"connection not found", fmt.Errorf("x: %w", config.ErrConnectionNotFound), ConnectionNotFound},
		{"unclassified", errors.New("boom"), Generic},
		{"interrupt", context.Canceled, Interrupted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// AsStatusError mimics the server interceptor; FromError mimics the CLI.
			wire := AsStatusError(tt.err)
			if got := FromError(wire); got != tt.want {
				t.Fatalf("round-trip FromError = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestFromError_LocalError verifies client-side sentinels (never crossing gRPC) are
// classified directly.
func TestFromError_LocalError(t *testing.T) {
	if got := FromError(fmt.Errorf("x: %w", config.ErrDaemonNotRunning)); got != DaemonNotRunning {
		t.Fatalf("local daemon-not-running = %d, want %d", got, DaemonNotRunning)
	}
	if got := FromError(nil); got != Success {
		t.Fatalf("nil = %d, want 0", got)
	}
	if got := FromError(errors.New("boom")); got != Generic {
		t.Fatalf("plain = %d, want %d", got, Generic)
	}
}

// TestFromError_CoarseGRPCFallback verifies a gRPC error lacking a kubevpn detail
// (e.g. from a library / older daemon) falls back to the coarse code map.
func TestFromError_CoarseGRPCFallback(t *testing.T) {
	tests := []struct {
		code codes.Code
		want int
	}{
		{codes.NotFound, NotFound},
		{codes.PermissionDenied, PermissionDenied},
		{codes.Unavailable, ClusterUnreachable},
		{codes.DeadlineExceeded, Timeout},
		{codes.AlreadyExists, AlreadyExists},
		{codes.Internal, Internal},
		{codes.Canceled, Interrupted},
		{codes.Unknown, Generic},
	}
	for _, tt := range tests {
		t.Run(tt.code.String(), func(t *testing.T) {
			if got := FromError(status.Error(tt.code, "x")); got != tt.want {
				t.Fatalf("fallback %v = %d, want %d", tt.code, got, tt.want)
			}
		})
	}
}
