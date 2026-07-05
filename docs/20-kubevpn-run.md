# KubeVPN Run Design

## 1. Overview

The run package (`pkg/run`) implements `kubevpn run` — running a Kubernetes workload locally as Docker containers. It fetches the workload's Pod spec from the cluster, converts it to Docker container configurations, connects to the cluster network, and starts containers locally.

## 2. Flow

```
kubevpn run deployment/myapp
  │
  ├─ 1. InitClient → K8s clientset
  ├─ 2. Connect → cluster network (host or container mode)
  ├─ 3. fetchPodContext → PodTemplateSpec + env + volumes + DNS
  ├─ 4. ConvertPodToContainerConfigList → Docker RunConfig list
  └─ 5. ConfigList.Run → start Docker containers
```

## 3. Connection Modes

`Options.Connect()` supports two modes:

### 3.1 Host Mode (`ConnectModeHost`)

Connects via the local daemon (same as `kubevpn connect`/`kubevpn proxy`):
- Calls daemon gRPC `Proxy()` to establish VPN tunnel on the host
- The Docker container uses the host network or shares network with the TUN device
- Registers `teardownRunProxy` as a rollback function (`connect.go`)

**Teardown (`teardownRunProxy`)** mirrors the proven `kubevpn proxy --foreground` order
(`cmd/kubevpn/cmds/proxy.go`): it **leaves the proxy first, then disconnects**. Leave runs on its own
`Leave` RPC, which the daemon resolves by `currentConnectionID` (set in the `Proxy` handler) — so it
reliably **unpatches the injected sidecars** regardless of how the manager/workload namespaces relate.
This matters because a bare disconnect-by-kubeconfig derives the connection ID from the *namespace*
(see [04-connection-id.md](04-connection-id.md)); relying on it alone left the sidecars patched. Both
the `Leave` and `Disconnect` streams render with the spinner + `✓` UX via `grpcutil.RenderGRPCStream`
(see [30-connect-progress.md](30-connect-progress.md)), and a leading blank line keeps the first step
off the terminal's `^C` echo. `--no-proxy` runs skip Leave (nothing was injected).

> **Port coverage caveat (host mode).** The dev container is a standalone container published on
> `host:<port>`, and the gvisor `LocalTCPForwarder` dials `127.0.0.1` (`pkg/core/gvisor_tcp_forwarder.go`).
> Under the unified mesh full-proxy, only the workload's **declared** ports are routed back to the
> local machine; an undeclared port (e.g. `9090`) falls through the envoy `:15006` capture to
> `origin_cluster` (the real app), so a run/sync e2e that probes `podIP:9090` will not reach the local
> dev process. Root cause and restore options: [17-sidecar-injection.md](17-sidecar-injection.md)
> ("Full-proxy port coverage"). The earlier `run -p` → proxy-portmap experiment (commits `087cc531` /
> `b68c3181`) was reverted, so run e2e is back to its pre-experiment behavior.

### 3.2 Container Mode (`ConnectModeContainer`)

Runs a KubeVPN container that handles the VPN connection:
- Creates a Docker container running `kubevpn connect` or `kubevpn proxy` with `--foreground`
- The container gets `--privileged` access and a KubeVPN Docker network
- The workload container uses `--network container:<vpn-container>` to share the VPN namespace
- Useful when running inside Docker-in-Docker or CI environments

## 4. PodContext — Cluster Data Fetching

`fetchPodContext()` gathers all data needed to replicate the pod locally:

```go
type PodContext struct {
    TemplateSpec *v1.PodTemplateSpec  // pod spec from controller
    EnvMap       map[string]string    // env vars from running pod
    MountVolume  map[string][]mount.Mount  // volume mounts
    DNSConfig    *dns.ClientConfig    // DNS configuration
}
```

Steps:
1. `GetPodTemplateSpec()` — resolve workload → top-level owner → extract template
2. `GetRunningPodList()` — find a running pod matching the template's labels
3. `GetEnv()` — exec into the pod to dump environment variables
4. `GetVolume()` — resolve volume sources (ConfigMap, Secret, PVC, etc.) to local Docker mounts
5. `GetDNS()` — get DNS resolver config from the pod

## 5. Pod-to-Docker Conversion (`runconfig.go`)

`ConvertPodToContainerConfigList()` converts the K8s PodTemplateSpec into a `ConfigList`:

**Container layout:**
- Index 0: "dev" container — the user's interactive container (runs in foreground)
- Index 1..N: sidecar containers (run detached)

**Conversion includes:**
- Image, command, args
- Environment variables (from pod + cluster)
- Port bindings (containerPort → hostPort)
- Volume mounts (ConfigMap/Secret → bind mounts, PVC → named volumes)
- DNS configuration
- Resource limits (not enforced in Docker by default)

**Docker networking:**
- The **last container** (typically a sidecar) owns the network namespace and joins the KubeVPN Docker network (`kubevpn-traffic-manager`)
- All other containers, including the dev container (index 0), attach via `--network container:<lastContainerName>` to share the network namespace

## 6. Container Lifecycle

`ConfigList.Run()` starts containers in **reverse order**: the network-owning sidecar first (detached), then other sidecars, then the dev container (foreground). This ensures the network namespace exists before other containers attach to it.

The dev container runs in the foreground until the user exits. On **Ctrl-C** the shared context is
cancelled, `exec.CommandContext` SIGKILLs `docker`, and `cmd.Wait()` returns `signal: killed`. Because
that kill is the *intended* shutdown, `ConfigList.Run()` treats a `RunContainer` error with a cancelled
`ctx.Err()` as a clean exit (returns `nil`) instead of surfacing a spurious
`signal: killed: docker run failed`. The rollback funcs still run, and the process exits `130`
(Interrupted) from the signal itself — see [31-exit-codes.md](31-exit-codes.md).

`ConfigList.Remove()` cleans up all containers (via `docker rm --force`) and disconnects from the Docker
network. It is gated on `--rm` (`hostConfig.AutoRemove`): without `--rm`, the run containers are left in
place, matching plain `docker run` semantics.

## 7. Security Options, Volumes & DinD Details

### 7.1 Security options (`docker_utils.go` / `runconfig.go`)

Every container gets `containerSecurityOpts()` so a locally-run workload behaves like it does in
the cluster (debuggers, mounts, profilers all work):

```
--cap-add SYS_PTRACE  --cap-add SYS_ADMIN
--security-opt apparmor=unconfined  --security-opt seccomp=unconfined
```

Additionally, if the source container's `SecurityContext.Privileged` is true, `--privileged` is
propagated. Containers run `--user root` with `LC_ALL=C.UTF-8`, the pod's `Subdomain` as
`--domainname`, the container's `WorkingDir`, and pod labels as `--label`s. `docker_opts.go`'s
`Parse` maps user-supplied `-v`/`--privileged`/`-p` flags into the Docker `Config`/`HostConfig`.

### 7.2 Volume conversion (`volume.go`)

`GetVolume` turns each non-sidecar container's `VolumeMounts` into Docker **bind mounts** by
*downloading* the in-cluster volume contents to a local temp dir (via `pkg/cp` with `MaxTries=10`)
and binding that dir at the same `MountPath`. Notes:

- The `vpn`/`envoy` sidecar containers are skipped.
- `MountPath == /tmp` is skipped (Docker manages its own).
- `SubPath` is appended to the local path so subpath mounts map correctly.
- Download failure is non-fatal (logged, mount skipped). `RemoveDir` cleans up the temp dirs on
  teardown.

This is a snapshot copy, not a live PVC bind — see [25-file-copy.md](25-file-copy.md) for the
underlying tar transfer.

### 7.3 Container-mode IPv6 & DinD (`connect.go`, `options.go`)

In **Container mode**, the kubevpn-in-Docker container is started `--privileged --rm` with the
security opts, `--sysctl net.ipv6.conf.all.disable_ipv6=0` (Docker disables IPv6 in the container
netns by default, which would break the dual-stack TUN — see
[38-ipv6-dual-stack.md](38-ipv6-dual-stack.md)), `host.docker.internal`/`kubernetes`
host-gateway entries, and joins the `kubevpn-traffic-manager` Docker network. Container mode is
auto-selected when the default mode runs **inside a container** — detected via
`incontainer.Detect()` in `options.go` (Docker-in-Docker), since a nested host-mode daemon cannot
own the host routes.

## 8. Related Files

| File | Purpose |
|------|---------|
| `pkg/run/options.go` | Options struct, Main, Run, PodContext, fetchPodContext |
| `pkg/run/connect.go` | Cluster connection (host/container mode) |
| `pkg/run/runconfig.go` | Pod→Docker conversion (RunConfig, ConfigList) |
| `pkg/run/docker_opts.go` | Docker CLI flag parsing (ContainerOptions) |
| `pkg/run/docker_utils.go` | Shared Docker helpers |
| `pkg/run/volume.go` | K8s volume → Docker mount conversion |
