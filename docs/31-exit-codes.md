# Exit Codes

## 1. Overview

The `kubevpn` CLI returns a fine-grained process exit code so scripts can tell failure
classes apart instead of branching on log text. `0` is success, `1` is an unclassified
error, `130` is interrupt (Ctrl-C), and the higher ranges group failures by domain:
`2x` config/input, `3x` permission, `4x` cluster, `5x` resource, `6x` data-plane,
`7x` traffic-manager, `8x` daemon, `9x` internal/cleanup, `10x` SSH, `11x` file sync,
`12x` Docker (`kubevpn run`), `14x` self-upgrade.

The single source of truth is `pkg/util/exitcode` (constants + `Classify`/`AsStatusError`/
`FromError`). `cmd/kubevpn/main.go` maps the command's returned error to a code via
`exitcode.FromError` and calls `os.Exit`.

## 2. Code Table

| Code | Name | Meaning |
|---|---|---|
| 0 | Success | Command completed |
| 1 | Generic | Unclassified error |
| 20 | KubeconfigInvalid | kubeconfig missing/malformed |
| 21 | KubeconfigWrongProtocol | server URL scheme is not http/https |
| 22 | KubeconfigUnresolvable | server hostname resolves to no IP |
| 23 | InvalidArgument | invalid flag/parameter |
| 31 | PermissionDenied | RBAC forbidden / needs root |
| 32 | Unauthenticated | invalid credentials (401) |
| 40 | ClusterUnreachable | API server / control-plane unreachable |
| 41 | PortForwardTimeout | port-forward did not become ready |
| 42 | Timeout | operation timed out |
| 43 | ControlPlaneNotServing | control-plane gRPC not serving |
| 50 | NotFound | workload/namespace/resource not found |
| 51 | ConnectionNotFound | no active VPN connection/session |
| 53 | AlreadyExists | resource/connection already exists |
| 60 | TunDeviceFailed | TUN device create/configure failed |
| 61 | RouteSetupFailed | route could not be added |
| 62 | DNSSetupFailed | DNS configuration failed |
| 63 | DHCPExhausted | TUN IP pool exhausted |
| 64 | TunIPConflict | no non-conflicting TUN IP after retries |
| 70 | TrafficManagerDeployFailed | traffic-manager resource creation failed |
| 71 | TrafficManagerTimeout | traffic-manager pod not ready in time |
| 72 | ImagePullFailed | traffic-manager image could not be pulled |
| 73 | EnvoyInjectFailed | sidecar injection failed |
| 80 | DaemonNotRunning | local kubevpn daemon not running |
| 81 | DaemonVersionMismatch | client/daemon/server versions incompatible |
| 90 | Internal | panic / internal bug |
| 92 | CleanupFailed | leave unpatch / reset restore / configmap rollback failed |
| 100 | SSHConnect | SSH dial/handshake/jump-host unreachable |
| 101 | SSHAuth | SSH auth rejected, or key file read/parse failed |
| 102 | SSHConfig | invalid `--ssh-jump` spec / circular ProxyJump |
| 103 | GSSAPI | Kerberos/GSSAPI (krb5.conf/keytab/ccache/token) failed |
| 104 | SSHRemoteCommand | remote command / remote kubeconfig fetch / SCP failed |
| 110 | SyncthingFailed | syncthing process/app start, API, or port allocation failed |
| 120 | DockerDaemonNotRunning | local Docker daemon unreachable (`kubevpn run`) |
| 121 | DockerImagePull | Docker image could not be pulled |
| 122 | DockerRunFailed | Docker container create/start/run failed |
| 130 | Interrupted | Ctrl-C / SIGINT |
| 140 | UpgradeNetworkFailed | GitHub API / release download network failure |
| 141 | UpgradeUnsupportedPlatform | no release asset matches the platform |
| 142 | UpgradeInstallFailed | extract/chmod/replace binary failed |

## 3. Classification Mechanism

Classification happens at two points and is carried two ways.

**Server-side (errors crossing the daemon gRPC boundary).** The classify interceptor
(`pkg/daemon/rpc/classifyinterceptor.go`) calls `exitcode.AsStatusError`, which tags the
gRPC status with a `google.golang.org/genproto/googleapis/rpc/errdetails.ErrorInfo`:
`Domain="kubevpn.io"`, `Reason` a readable token (e.g. `DHCP_EXHAUSTED`), and
`Metadata["exitCode"]` the numeric code. Carrying the code in the status *detail* — rather
than the gRPC status code — lets the taxonomy exceed the 16 standard gRPC codes. The gRPC
code is still set as a coarse fallback. `FromError` reads the detail first, then falls back
to mapping the coarse code.

**Client-side (errors that never cross gRPC).** Failures like "daemon not running", a
version mismatch, `kubevpn run` Docker errors, and `kubevpn upgrade` are detected in the
CLI process itself. `FromError` classifies these local errors directly via `errors.Is`
against the sentinels in `pkg/config/errors.go`.

**Text-matched classification.** Two cases lack typed errors and are classified by message
text at the boundary, then wrapped with a sentinel: SSH connect-vs-auth-vs-GSSAPI
(`wrapDialError` in `pkg/ssh`, since `x/crypto/ssh` exposes no typed auth error) and the
Docker daemon-down/image-pull/run split (`classifyDockerError` in `pkg/util/docker.go`,
which captures the `docker` CLI stderr via an `io.MultiWriter` while still streaming it live).

**Two-daemon preservation.** A failure in the root daemon is classified there, sent to the
user daemon, and forwarded to the CLI. `AsStatusError` treats an already-classified error
(one already carrying a kubevpn `ErrorInfo`, or any non-`Unknown` gRPC code such as a
panic's `Internal`) as a pass-through, so the original code survives the user→root hop.

## 4. Adding a New Code

1. Add the constant to `pkg/util/exitcode/exitcode.go` (keep the existing values stable).
2. Add a sentinel to `pkg/config/errors.go` (or rely on a Kubernetes `apierrors.IsXxx`
   matcher already handled in `Classify`).
3. Add a rule to the `rules` table in `exitcode.go` mapping the sentinel to the code, a
   coarse gRPC code, and a reason token.
4. Wrap the originating error with `%w` against the sentinel at its handler boundary.
5. Cover it in `pkg/util/exitcode/exitcode_test.go` and, if it crosses gRPC, in
   `pkg/daemon/rpc/classify_integration_test.go`.

See also: [14-rpc-daemon-mapping.md](14-rpc-daemon-mapping.md) for the RPC→daemon routing,
and [02-dual-daemon.md](02-dual-daemon.md) for the dual-daemon model.
