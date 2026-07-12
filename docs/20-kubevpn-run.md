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
- Registers a rollback function to `Disconnect()` on cleanup

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
- All containers join the KubeVPN Docker network (`kubevpn-traffic-manager`)
- Sidecar containers use `--network container:<dev>` to share namespace

## 6. Container Lifecycle

`ConfigList.Run()` starts containers in **reverse order**: sidecars first (detached), then the dev container (foreground). This ensures sidecars are ready before the main application starts.

`ConfigList.Remove()` cleans up all containers and disconnects from the Docker network.

## 7. Related Files

| File | Purpose |
|------|---------|
| `pkg/run/options.go` | Options struct, Main, Run, PodContext, fetchPodContext |
| `pkg/run/connect.go` | Cluster connection (host/container mode) |
| `pkg/run/runconfig.go` | Pod→Docker conversion (RunConfig, ConfigList) |
| `pkg/run/docker_opts.go` | Docker CLI flag parsing (ContainerOptions) |
| `pkg/run/docker_utils.go` | Shared Docker helpers |
| `pkg/run/volume.go` | K8s volume → Docker mount conversion |
