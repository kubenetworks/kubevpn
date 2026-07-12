# Dev Mode Design

## 1. Overview

Dev mode (`kubevpn dev`) is a composite command that combines cluster connection, optional traffic interception (proxy), and local Docker container execution. It is not a separate implementation but an orchestration of existing subsystems: `kubevpn run` (local Docker execution) with optional proxy capabilities.

## 2. Relationship to Other Modes

```
kubevpn connect   → VPN tunnel only (network access)
kubevpn proxy     → VPN + sidecar injection (traffic interception)
kubevpn sync      → VPN + clone workload + file sync
kubevpn run       → VPN + proxy + run workload locally in Docker
kubevpn dev       → alias-based composition of the above
```

Dev mode uses the **alias system** (`cmd/kubevpn/cmds/alias.go`) to compose existing commands. Users configure aliases in `~/.kubevpn/config.yaml` that chain multiple KubeVPN operations.

## 3. Core Workflow

A typical dev workflow combines `kubevpn run` with project-specific options:

```
kubevpn run deployment/myapp \
  --connect-mode container \
  --headers user=alice \
  --docker-image myapp:dev \
  --entrypoint "npm run dev" \
  --port 3000:3000
```

This executes:

1. **Connect**: Establish VPN tunnel to cluster (host or container mode)
2. **Proxy** (optional): Inject sidecar into workload for header-based traffic splitting
3. **Fetch**: Get PodTemplateSpec, env vars, volumes, DNS from running pod
4. **Convert**: Transform K8s Pod spec to Docker container config
5. **Run**: Start Docker containers locally with cluster network access

## 4. Connect Modes for Dev

### 4.1 Host Mode

The VPN runs on the host machine via the daemon. The Docker container accesses cluster services through the host's TUN device. Best for native development (IDE debugging, local tools).

### 4.2 Container Mode

A separate Docker container runs `kubevpn connect/proxy`. The dev container shares its network namespace (`--network container:<vpn>`). Best for Docker-in-Docker environments or CI.

## 5. Container Execution (`kubevpn run`)

See `docs/20-kubevpn-run.md` for the full run design. Key points for dev mode:

- **Dev container** (index 0): Runs in foreground, interactive, with user-specified entrypoint
- **Sidecar containers** (index 1..N): Run detached in shared network namespace
- Environment variables, volumes, and DNS are replicated from the cluster
- Docker network `kubevpn-traffic-manager` provides inter-container connectivity

## 6. File Sync in Dev

For file synchronization during development, combine with `kubevpn sync` or mount local directories as Docker volumes:

```
kubevpn run deployment/myapp \
  --volume ./src:/app/src \
  --entrypoint "npm run dev --watch"
```

For syncthing-based sync, use `kubevpn sync` (see `docs/26-sync-mode.md`).

## 7. Related Files

| File | Purpose |
|------|---------|
| `pkg/run/options.go` | Options, Main, Run, PodContext |
| `pkg/run/connect.go` | Host/container connect modes |
| `pkg/run/runconfig.go` | Pod→Docker conversion |
| `cmd/kubevpn/cmds/run.go` | Run CLI command |
| `cmd/kubevpn/cmds/alias.go` | Alias system for composing commands |
| `docs/20-kubevpn-run.md` | Run mode design |
| `docs/26-sync-mode.md` | Sync mode design |
