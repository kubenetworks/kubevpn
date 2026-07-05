// Package exitcode maps daemon/command errors to a fine-grained, stable set of
// process exit codes so scripts invoking the kubevpn CLI can tell failure classes
// apart.
//
// Classification is carried two ways:
//   - Across the daemon gRPC boundary: the server's classify interceptor calls
//     AsStatusError, which tags the gRPC status with an errdetails.ErrorInfo whose
//     Metadata["exitCode"] holds the rich code (and Reason a readable token). This
//     escapes the 16-value limit of standard gRPC codes. The gRPC code itself is
//     still set as a coarse fallback. FromError reads the detail first.
//   - Client-side (errors never crossing gRPC, e.g. "daemon not running"): FromError
//     classifies the local error directly via errors.Is.
package exitcode

import (
	"context"
	"errors"
	"strconv"

	errdetails "google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// Process exit codes, grouped by domain. Values are part of the CLI contract; keep
// them stable. See docs/31-exit-codes.md.
const (
	Success = 0
	Generic = 1

	// config / input (20-29)
	KubeconfigInvalid       = 20
	KubeconfigWrongProtocol = 21
	KubeconfigUnresolvable  = 22
	InvalidArgument         = 23

	// permission (30-39)
	PermissionDenied = 31
	Unauthenticated  = 32

	// cluster reachability (40-49)
	ClusterUnreachable = 40
	PortForwardTimeout = 41
	Timeout            = 42
	XDSNotServing      = 43

	// resource state (50-59)
	NotFound           = 50
	ConnectionNotFound = 51
	AlreadyExists      = 53

	// data-plane networking (60-69)
	TunDeviceFailed  = 60
	RouteSetupFailed = 61
	DNSSetupFailed   = 62
	DHCPExhausted    = 63
	TunIPConflict    = 64

	// traffic-manager / image (70-79)
	TrafficManagerDeployFailed = 70
	TrafficManagerTimeout      = 71
	ImagePullFailed            = 72
	EnvoyInjectFailed          = 73

	// daemon lifecycle (80-89)
	DaemonNotRunning      = 80
	DaemonVersionMismatch = 81

	// internal (90)
	Internal = 90
	// cleanup / rollback (92)
	CleanupFailed = 92

	// SSH (100-109)
	SSHConnect       = 100
	SSHAuth          = 101
	SSHConfig        = 102
	GSSAPI           = 103
	SSHRemoteCommand = 104

	// file sync (110-119)
	SyncthingFailed = 110

	// docker / kubevpn run (120-129)
	DockerDaemonNotRunning = 120
	DockerImagePull        = 121
	DockerRunFailed        = 122

	// self-upgrade (140-149)
	UpgradeNetworkFailed       = 140
	UpgradeUnsupportedPlatform = 141
	UpgradeInstallFailed       = 142

	// interrupt
	Interrupted = 130
)

const (
	errorInfoDomain = "kubevpn.io"
	metaExitCode    = "exitCode"
)

// is returns a matcher that reports whether err wraps target.
func is(target error) func(error) bool {
	return func(err error) bool { return errors.Is(err, target) }
}

// classification rules, evaluated in order; first match wins. kubevpn sentinels are
// checked before the generic Kubernetes apierrors matchers so a more specific code
// wins (e.g. ConnectionNotFound before the generic NotFound).
var rules = []struct {
	match  func(error) bool
	code   int
	grpc   codes.Code
	reason string
}{
	// security first: a permission/auth failure is the most actionable signal and must
	// win even when a broad wrapper (e.g. ErrTrafficManagerDeploy) also matches.
	{apierrors.IsForbidden, PermissionDenied, codes.PermissionDenied, "RBAC_FORBIDDEN"},
	{apierrors.IsUnauthorized, Unauthenticated, codes.Unauthenticated, "UNAUTHENTICATED"},

	// config / input
	{is(config.ErrKubeconfigWrongProtocol), KubeconfigWrongProtocol, codes.InvalidArgument, "KUBECONFIG_WRONG_PROTOCOL"},
	{is(config.ErrKubeconfigUnresolvable), KubeconfigUnresolvable, codes.InvalidArgument, "KUBECONFIG_UNRESOLVABLE"},
	{is(config.ErrInvalidKubeconfig), KubeconfigInvalid, codes.InvalidArgument, "KUBECONFIG_INVALID"},

	// daemon lifecycle (client-side)
	{is(config.ErrDaemonNotRunning), DaemonNotRunning, codes.Unavailable, "DAEMON_NOT_RUNNING"},
	{is(config.ErrDaemonVersionMismatch), DaemonVersionMismatch, codes.FailedPrecondition, "DAEMON_VERSION_MISMATCH"},

	// cluster reachability
	{is(config.ErrPortForwardTimeout), PortForwardTimeout, codes.Unavailable, "PORT_FORWARD_TIMEOUT"},
	{is(config.ErrXDSNotServing), XDSNotServing, codes.Unavailable, "XDS_NOT_SERVING"},

	// data-plane networking
	{is(config.ErrTunDeviceFailed), TunDeviceFailed, codes.Aborted, "TUN_DEVICE_FAILED"},
	{is(config.ErrRouteSetupFailed), RouteSetupFailed, codes.Aborted, "ROUTE_SETUP_FAILED"},
	{is(config.ErrDNSSetupFailed), DNSSetupFailed, codes.Aborted, "DNS_SETUP_FAILED"},
	{is(config.ErrDHCPExhausted), DHCPExhausted, codes.ResourceExhausted, "DHCP_EXHAUSTED"},
	{is(config.ErrTunIPConflict), TunIPConflict, codes.Aborted, "TUN_IP_CONFLICT"},

	// traffic-manager / image
	{is(config.ErrImagePull), ImagePullFailed, codes.FailedPrecondition, "IMAGE_PULL_FAILED"},
	{is(config.ErrTrafficManagerTimeout), TrafficManagerTimeout, codes.DeadlineExceeded, "TRAFFIC_MANAGER_TIMEOUT"},
	{is(config.ErrTrafficManagerDeploy), TrafficManagerDeployFailed, codes.Aborted, "TRAFFIC_MANAGER_DEPLOY_FAILED"},
	{is(config.ErrEnvoyInject), EnvoyInjectFailed, codes.Aborted, "ENVOY_INJECT_FAILED"},

	// resource state (kubevpn sentinels first)
	{is(config.ErrConnectionNotFound), ConnectionNotFound, codes.NotFound, "CONNECTION_NOT_FOUND"},
	{is(config.ErrNotFound), NotFound, codes.NotFound, "NOT_FOUND"},
	{is(config.ErrPermissionDenied), PermissionDenied, codes.PermissionDenied, "PERMISSION_DENIED"},

	// SSH
	{is(config.ErrGSSAPI), GSSAPI, codes.Unauthenticated, "GSSAPI"},
	{is(config.ErrSSHAuth), SSHAuth, codes.Unauthenticated, "SSH_AUTH"},
	{is(config.ErrSSHConfig), SSHConfig, codes.InvalidArgument, "SSH_CONFIG"},
	{is(config.ErrSSHRemoteCommand), SSHRemoteCommand, codes.Aborted, "SSH_REMOTE_COMMAND"},
	{is(config.ErrSSHConnect), SSHConnect, codes.Unavailable, "SSH_CONNECT"},

	// file sync
	{is(config.ErrSyncthing), SyncthingFailed, codes.Internal, "SYNCTHING_FAILED"},

	// docker (kubevpn run)
	{is(config.ErrDockerDaemonNotRunning), DockerDaemonNotRunning, codes.Unavailable, "DOCKER_DAEMON_NOT_RUNNING"},
	{is(config.ErrDockerImagePull), DockerImagePull, codes.FailedPrecondition, "DOCKER_IMAGE_PULL"},
	{is(config.ErrDockerRun), DockerRunFailed, codes.Aborted, "DOCKER_RUN_FAILED"},

	// self-upgrade
	{is(config.ErrUpgradeNetwork), UpgradeNetworkFailed, codes.Unavailable, "UPGRADE_NETWORK_FAILED"},
	{is(config.ErrUpgradeUnsupportedPlatform), UpgradeUnsupportedPlatform, codes.InvalidArgument, "UPGRADE_UNSUPPORTED_PLATFORM"},
	{is(config.ErrUpgradeInstall), UpgradeInstallFailed, codes.Aborted, "UPGRADE_INSTALL_FAILED"},

	// cleanup / rollback
	{is(config.ErrCleanupFailed), CleanupFailed, codes.Aborted, "CLEANUP_FAILED"},

	// invalid user input (checked late so a more specific code wins)
	{is(config.ErrInvalidArgument), InvalidArgument, codes.InvalidArgument, "INVALID_ARGUMENT"},

	// context
	{is(context.Canceled), Interrupted, codes.Canceled, "INTERRUPTED"},
	{is(context.DeadlineExceeded), Timeout, codes.DeadlineExceeded, "TIMEOUT"},

	// Kubernetes apierrors (auto-recognized, unwrap %w)
	{apierrors.IsAlreadyExists, AlreadyExists, codes.AlreadyExists, "ALREADY_EXISTS"},
	{apierrors.IsNotFound, NotFound, codes.NotFound, "NOT_FOUND"},
	{apierrors.IsServiceUnavailable, ClusterUnreachable, codes.Unavailable, "CLUSTER_UNREACHABLE"},
	{func(e error) bool {
		return apierrors.IsTimeout(e) || apierrors.IsServerTimeout(e) || apierrors.IsTooManyRequests(e)
	}, Timeout, codes.DeadlineExceeded, "TIMEOUT"},
	{apierrors.IsInternalError, Internal, codes.Internal, "INTERNAL"},
}

// Classify maps err to its exit code, the coarse gRPC code to carry it, and a
// readable reason token. An unrecognized error is (Generic, codes.Unknown, "").
func Classify(err error) (code int, grpc codes.Code, reason string) {
	if err == nil {
		return Success, codes.OK, ""
	}
	for _, r := range rules {
		if r.match(err) {
			return r.code, r.grpc, r.reason
		}
	}
	return Generic, codes.Unknown, ""
}

// AsStatusError converts a handler error into a gRPC status error tagged with the
// rich exit code (via errdetails.ErrorInfo). It is a no-op for nil and for errors
// already carrying a classification — a kubevpn ErrorInfo detail, or any non-Unknown
// gRPC code (e.g. a panic's Internal, or an error forwarded from another daemon) —
// so classification is set once and preserved across the user→root daemon hop.
func AsStatusError(err error) error {
	if err == nil {
		return nil
	}
	if st, ok := status.FromError(err); ok && (st.Code() != codes.Unknown || hasKubeVPNDetail(st)) {
		return err
	}
	code, grpc, reason := Classify(err)
	st := status.New(grpc, err.Error())
	withDetails, derr := st.WithDetails(&errdetails.ErrorInfo{
		Domain:   errorInfoDomain,
		Reason:   reason,
		Metadata: map[string]string{metaExitCode: strconv.Itoa(code)},
	})
	if derr != nil {
		return st.Err()
	}
	return withDetails.Err()
}

// FromError maps an error returned from command execution to a process exit code.
// Real gRPC errors are read from the rich ErrorInfo detail when present, else from
// the coarse gRPC code. Local (non-gRPC) errors are classified directly so that
// client-side sentinels like ErrDaemonNotRunning surface their code.
func FromError(err error) int {
	if err == nil {
		return Success
	}
	if st, ok := status.FromError(err); ok {
		if code, found := exitCodeFromDetails(st); found {
			return code
		}
		return fromGRPCCode(st.Code())
	}
	code, _, _ := Classify(err)
	return code
}

func hasKubeVPNDetail(st *status.Status) bool {
	_, found := exitCodeFromDetails(st)
	return found
}

func exitCodeFromDetails(st *status.Status) (int, bool) {
	if st == nil {
		return 0, false
	}
	for _, d := range st.Details() {
		info, ok := d.(*errdetails.ErrorInfo)
		if !ok || info.GetDomain() != errorInfoDomain {
			continue
		}
		if v, ok := info.GetMetadata()[metaExitCode]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// fromGRPCCode is the coarse fallback for gRPC errors lacking a kubevpn detail
// (e.g. from a client library or an older daemon).
func fromGRPCCode(c codes.Code) int {
	switch c {
	case codes.OK:
		return Success
	case codes.Canceled:
		return Interrupted
	case codes.InvalidArgument, codes.OutOfRange:
		return InvalidArgument
	case codes.FailedPrecondition:
		return InvalidArgument
	case codes.PermissionDenied:
		return PermissionDenied
	case codes.Unauthenticated:
		return Unauthenticated
	case codes.Unavailable:
		return ClusterUnreachable
	case codes.DeadlineExceeded:
		return Timeout
	case codes.NotFound:
		return NotFound
	case codes.AlreadyExists:
		return AlreadyExists
	case codes.ResourceExhausted:
		return DHCPExhausted
	case codes.Internal:
		return Internal
	default:
		return Generic
	}
}
