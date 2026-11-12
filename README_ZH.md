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

[English](README.md) | [中文](README_ZH.md) | [使用文档](https://kubevpn.dev/zh) | [架构设计](docs/README.md)

KubeVPN 提供云原生开发环境，无缝连接 Kubernetes 集群网络。

- 使用 Pod/Service IP 或 Kubernetes DNS 直接访问集群网络
- 通过服务网格将集群服务的入站流量拦截到本地
- 在本地 Docker 中运行 Kubernetes Pod，环境、卷和网络与集群完全一致

## 前置条件

- 可用的 kubeconfig，以及目标命名空间的相应 RBAC 权限
- 本地 sudo/root 权限（用于创建 TUN 设备和修改路由/DNS）
- `unzip`（脚本安装方式需要）

## 安装

选择一种方式：

```bash
# 脚本（macOS/Linux）
curl -fsSL https://kubevpn.dev/install.sh | sh

# Homebrew（macOS/Linux）
brew install kubevpn

# Snap（Linux）
sudo snap install kubevpn

# Krew（Windows/macOS/Linux）
kubectl krew index add kubevpn https://github.com/kubenetworks/kubevpn.git
kubectl krew install kubevpn/kubevpn

# Scoop（Windows）
scoop bucket add extras
scoop install kubevpn
```

或从 [GitHub Releases](https://github.com/kubenetworks/kubevpn/releases/latest) 下载。

## 快速开始

1. 部署示例 Bookinfo 应用：

```bash
kubectl apply -f https://raw.githubusercontent.com/kubenetworks/kubevpn/master/samples/bookinfo.yaml
```

2. 连接到集群网络：

```bash
kubevpn connect
```

3. 查看连接状态：

```bash
kubevpn status
```

4. 访问集群服务：

```bash
# 使用 Pod IP
kubectl get pods -o wide
curl <pod-ip>:9080

# 使用 Service IP
kubectl get svc -o wide
curl <service-ip>:9080

# 使用 Kubernetes DNS
curl productpage.default.svc.cluster.local:9080
curl productpage:9080  # 同一命名空间下可用短名
```

5. 清理（重要：请按此顺序）：

```bash
# 1. 如果使用过 `kubevpn proxy` 或 `kubevpn sync`，先离开这些资源
#    这会将工作负载恢复到原始状态并移除注入的 sidecar
kubevpn leave deployments/productpage

# 2. 按连接 ID 断开连接（从 `kubevpn status` 获取）
kubevpn disconnect <connection-id>

# 3. 删除示例 Bookinfo
kubectl delete -f https://raw.githubusercontent.com/kubenetworks/kubevpn/master/samples/bookinfo.yaml
```

可选：若要从集群彻底清理 KubeVPN traffic manager：

```bash
kubevpn uninstall
# 警告：这会影响该集群的所有用户！
```

## 安全提示

- KubeVPN 会修改本地 TUN 设备、路由表和 DNS 设置
- 代理模式会向工作负载注入 Sidecar 容器，可能需要 `privileged`、`NET_ADMIN` 或 `NET_RAW` 权限
- 对于生产集群，使用前请了解 RBAC 和网络影响
- 了解更多：[安全文档](https://kubevpn.dev/zh/docs/operations/security)

## 下一步

- 完整文档：[kubevpn.dev](https://kubevpn.dev/zh)
- 命令参考：[Connect](https://kubevpn.dev/zh/docs/commands/kubevpn_connect)
- 架构与设计文档：[docs/README.md](docs/README.md)

## 贡献

见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 支持

[![JetBrains logo.](https://resources.jetbrains.com/storage/products/company/brand/logos/jetbrains.svg)](https://jb.gg/OpenSourceSupport)
