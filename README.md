![kubevpn](https://raw.githubusercontent.com/wencaiwulue/kubevpn/master/samples/flat_log.png)

[![GitHub Workflow][1]](https://github.com/kubenetworks/kubevpn/actions)
[![Go Version][2]](https://github.com/kubenetworks/kubevpn/blob/master/go.mod)
[![Go Report][3]](https://goreportcard.com/report/github.com/wencaiwulue/kubevpn)
[![Maintainability][4]](https://qlty.sh/gh/kubenetworks/projects/kubevpn)
[![GitHub License][5]](https://github.com/kubenetworks/kubevpn/blob/master/LICENSE)
[![Docker Pulls][6]](https://hub.docker.com/r/naison/kubevpn)
[![Releases][7]](https://github.com/kubenetworks/kubevpn/releases)
[![GoDoc](https://godoc.org/github.com/kubenetworks/kubevpn?status.png)](https://pkg.go.dev/github.com/wencaiwulue/kubevpn/v2)
[![codecov](https://codecov.io/gh/wencaiwulue/kubevpn/graph/badge.svg?token=KMDSINSDEP)](https://codecov.io/gh/wencaiwulue/kubevpn)
[![Snapcraft](https://snapcraft.io/kubevpn/badge.svg)](https://snapcraft.io/kubevpn)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/kubenetworks/kubevpn)

[1]: https://img.shields.io/github/actions/workflow/status/kubenetworks/kubevpn/release.yml?logo=github
[2]: https://img.shields.io/github/go-mod/go-version/kubenetworks/kubevpn?logo=go
[3]: https://goreportcard.com/badge/github.com/wencaiwulue/kubevpn?style=flat
[4]: https://qlty.sh/gh/kubenetworks/projects/kubevpn/maintainability.svg
[5]: https://img.shields.io/github/license/kubenetworks/kubevpn
[6]: https://img.shields.io/docker/pulls/naison/kubevpn?logo=docker
[7]: https://img.shields.io/github/v/release/kubenetworks/kubevpn?logo=smartthings

# KubeVPN

[English](README.md) | [中文](README_ZH.md) | [Documentation](https://kubevpn.dev) | [Architecture](docs/README.md)

KubeVPN offers a Cloud-Native Dev Environment that seamlessly connects to your Kubernetes cluster network.

- Connect directly to cluster network using Pod/Service IP or Kubernetes DNS
- Intercept inbound traffic from cluster services to your local machine with service mesh
- Run Kubernetes pods locally in Docker with identical environment, volumes, and network

## Prerequisites

- Valid kubeconfig with appropriate RBAC permissions for target namespace
- Local sudo/root privileges (for creating TUN device and modifying routing/DNS)
- `unzip` (required for script-based installation)

## Installation

Choose one method:

```bash
# Script (macOS/Linux)
curl -fsSL https://kubevpn.dev/install.sh | sh

# Homebrew (macOS/Linux)
brew install kubevpn

# Snap (Linux)
sudo snap install kubevpn

# Krew (Windows/macOS/Linux)
kubectl krew index add kubevpn https://github.com/kubenetworks/kubevpn.git
kubectl krew install kubevpn/kubevpn

# Scoop (Windows)
scoop bucket add extras
scoop install kubevpn
```

Or download from [GitHub Releases](https://github.com/kubenetworks/kubevpn/releases/latest).

## Quick Start

1. Deploy the sample Bookinfo application:

```bash
kubectl apply -f https://raw.githubusercontent.com/kubenetworks/kubevpn/master/samples/bookinfo.yaml
```

2. Connect to the cluster network:

```bash
kubevpn connect
```

3. Check connection status:

```bash
kubevpn status
```

4. Access cluster services:

```bash
# Using Pod IP
kubectl get pods -o wide
curl <pod-ip>:9080

# Using Service IP
kubectl get svc -o wide
curl <service-ip>:9080

# Using Kubernetes DNS
curl productpage.default.svc.cluster.local:9080
curl productpage:9080  # short name works in same namespace
```

5. Clean up (IMPORTANT: follow this order):

```bash
# 1. If you used `kubevpn proxy` or `kubevpn sync`, leave those resources first
#    This restores the workloads to their original state and removes injected sidecars
kubevpn leave deployments/productpage

# 2. Disconnect by connection ID (from `kubevpn status`)
kubevpn disconnect <connection-id>

# 3. Delete the sample Bookinfo
kubectl delete -f https://raw.githubusercontent.com/kubenetworks/kubevpn/master/samples/bookinfo.yaml
```

Optional: To clean up KubeVPN traffic manager from the cluster entirely:

```bash
kubevpn uninstall
# WARNING: This affects all users of this cluster!
```

## Security Notice

- KubeVPN modifies your local TUN device, routing table, and DNS settings
- Proxy mode injects sidecar containers into workloads, which may require `privileged`, `NET_ADMIN`, or `NET_RAW` capabilities
- For production clusters, understand the RBAC and network impacts before use
- Learn more: [Security Documentation](https://kubevpn.dev/docs/operations/security)

## Example Commands

Disconnect from a specific connection ID:

```shell cli-checked
kubevpn disconnect <connection-id>
```

Disconnect from all connections:

```shell cli-checked
kubevpn disconnect --all
```

## Next Steps

- Full documentation: [kubevpn.dev](https://kubevpn.dev)
- Command reference: [Connect](https://kubevpn.dev/docs/commands/kubevpn_connect)
- Architecture and design docs: [docs/README.md](docs/README.md)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Supported by

[![JetBrains logo.](https://resources.jetbrains.com/storage/products/company/brand/logos/jetbrains.svg)](https://jb.gg/OpenSourceSupport)
