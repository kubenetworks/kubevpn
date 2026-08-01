package config

import "errors"

// Sentinel errors recognized by the exit-code classifier (pkg/util/exitcode).
// Wrap a concrete failure with %w against one of these so the CLI surfaces a
// specific process exit code. The classifier also auto-recognizes Kubernetes
// apierrors (NotFound/Forbidden/AlreadyExists/Timeout/...), so wrapping is only
// needed for non-k8s failures or to override the default classification.
var (
	// --- config / input ---

	// ErrInvalidArgument indicates user-supplied input is invalid (bad flag/format/spec).
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrInvalidKubeconfig indicates the provided kubeconfig file is missing or malformed.
	ErrInvalidKubeconfig = errors.New("kubeconfig is invalid")
	// ErrKubeconfigWrongProtocol indicates the kubeconfig server URL uses an unsupported scheme.
	ErrKubeconfigWrongProtocol = errors.New("kubeconfig server uses wrong protocol")
	// ErrKubeconfigUnresolvable indicates the kubeconfig server hostname resolves to no IP.
	ErrKubeconfigUnresolvable = errors.New("kubeconfig server hostname is unresolvable")

	// --- permission ---

	// ErrPermissionDenied indicates a privilege/credential failure (not root, RBAC forbidden).
	ErrPermissionDenied = errors.New("permission denied")

	// --- cluster reachability ---

	// ErrPortForwardTimeout indicates a kubectl port-forward did not become ready in time.
	ErrPortForwardTimeout = errors.New("port-forward readiness timeout")
	// ErrControlPlaneNotServing indicates the traffic-manager control-plane gRPC is not serving.
	ErrControlPlaneNotServing = errors.New("control plane not serving")

	// --- resource state ---

	// ErrNotFound indicates a required cluster resource (workload/namespace/...) is missing.
	ErrNotFound = errors.New("not found")
	// ErrConnectionNotFound indicates there is no active VPN connection/session for the request.
	ErrConnectionNotFound = errors.New("no connection found")

	// --- data-plane networking ---

	// ErrTunDeviceFailed indicates the TUN device could not be created or configured.
	ErrTunDeviceFailed = errors.New("tun device setup failed")
	// ErrRouteSetupFailed indicates a route could not be added to the routing table.
	ErrRouteSetupFailed = errors.New("route setup failed")
	// ErrDNSSetupFailed indicates DNS configuration failed.
	ErrDNSSetupFailed = errors.New("dns setup failed")
	// ErrDHCPExhausted indicates the DHCP TUN IP pool has no free address.
	ErrDHCPExhausted = errors.New("dhcp ip pool exhausted")
	// ErrTunIPConflict indicates no non-conflicting TUN IP could be allocated after retries.
	ErrTunIPConflict = errors.New("tun ip conflict")

	// --- traffic-manager / image ---

	// ErrTrafficManagerDeploy indicates a traffic-manager resource could not be created.
	ErrTrafficManagerDeploy = errors.New("traffic manager deploy failed")
	// ErrTrafficManagerTimeout indicates the traffic-manager pod did not become ready in time.
	ErrTrafficManagerTimeout = errors.New("traffic manager readiness timeout")
	// ErrImagePull indicates the traffic-manager image could not be pulled.
	ErrImagePull = errors.New("image pull failed")
	// ErrEnvoyInject indicates Envoy sidecar injection failed.
	ErrEnvoyInject = errors.New("envoy sidecar injection failed")

	// --- daemon lifecycle (detected client-side) ---

	// ErrDaemonNotRunning indicates the local kubevpn daemon is not running.
	ErrDaemonNotRunning = errors.New("daemon not running")
	// ErrDaemonVersionMismatch indicates the client/daemon/server versions are incompatible.
	ErrDaemonVersionMismatch = errors.New("daemon version mismatch")

	// --- cleanup / rollback ---

	// ErrCleanupFailed indicates a proxy unpatch / reset restore / configmap rollback failed.
	ErrCleanupFailed = errors.New("cleanup failed")

	// --- SSH ---

	// ErrSSHConnect indicates an SSH dial/handshake/jump-host reachability failure.
	ErrSSHConnect = errors.New("ssh connection failed")
	// ErrSSHAuth indicates an SSH authentication failure (password/key rejected, key read/parse).
	ErrSSHAuth = errors.New("ssh authentication failed")
	// ErrSSHConfig indicates an invalid SSH jump spec / config (jump flags, circular ProxyJump).
	ErrSSHConfig = errors.New("ssh config invalid")
	// ErrGSSAPI indicates a GSSAPI/Kerberos failure (krb5.conf/keytab/ccache/login/token).
	ErrGSSAPI = errors.New("gssapi authentication failed")
	// ErrSSHRemoteCommand indicates a remote command / remote kubeconfig fetch / SCP failure.
	ErrSSHRemoteCommand = errors.New("ssh remote command failed")

	// --- file sync (syncthing) ---

	// ErrSyncthing indicates a syncthing process/app start, API, or port-allocation failure.
	ErrSyncthing = errors.New("syncthing failed")

	// --- docker (kubevpn run) ---

	// ErrDockerDaemonNotRunning indicates the local Docker daemon is unreachable.
	ErrDockerDaemonNotRunning = errors.New("docker daemon not running")
	// ErrDockerImagePull indicates a Docker image could not be pulled.
	ErrDockerImagePull = errors.New("docker image pull failed")
	// ErrDockerRun indicates a Docker container create/start/run failure.
	ErrDockerRun = errors.New("docker run failed")

	// --- self-upgrade ---

	// ErrUpgradeNetwork indicates a GitHub API / release download network failure.
	ErrUpgradeNetwork = errors.New("upgrade network failed")
	// ErrUpgradeUnsupportedPlatform indicates no release asset matches the current platform.
	ErrUpgradeUnsupportedPlatform = errors.New("upgrade unsupported platform")
	// ErrUpgradeInstall indicates extracting/replacing the binary failed.
	ErrUpgradeInstall = errors.New("upgrade install failed")
)
