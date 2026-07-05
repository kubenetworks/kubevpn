# pkg/ 目录结构整理方案

## 现状

```
pkg/
├── config/          (4 files,  477 lines) ── 常量、配置、路径函数
├── controlplane/    (5 files,  853 lines) ── Envoy xDS 控制面
├── core/           (22 files, 2473 lines) ── TUN 网络协议核心
├── cp/              (3 files,  640 lines) ── kubectl cp 等价实现
├── daemon/          (2 files,  425 lines) ── gRPC daemon 入口
│   ├── action/     (20 files, 1817 lines) ── RPC handler 实现
│   ├── elevate/     (5 files,  277 lines) ── 权限提升
│   ├── handler/     (1 file,   434 lines) ── WebSocket SSH 终端
│   └── rpc/         (4 files, 5369 lines) ── protobuf 生成（勿改）
├── dhcp/            (2 files,  290 lines) ── DHCP IP 分配
├── dns/             (7 files,  946 lines) ── DNS 配置（跨平台）
├── driver/          (1 file,    65 lines) ── TUN 驱动管理
│   ├── openvpn/     (2 files,   44 lines) ── OpenVPN TAP 驱动
│   └── wintun/      (6 files,  110 lines) ── WireGuard TUN 驱动
├── handler/        (21 files, 3406 lines) ── 核心业务逻辑
├── inject/          (6 files,  867 lines) ── Sidecar 注入策略
├── localproxy/      (6 files,  706 lines) ── SOCKS5/HTTP CONNECT 代理
├── log/             (2 files,  239 lines) ── 结构化日志
├── run/             (5 files, 1068 lines) ── kubevpn run（Docker）
├── ssh/             (9 files, 1956 lines) ── SSH 客户端
├── syncthing/       (2 files,  297 lines) ── Syncthing 集成
│   └── auto/        (1 file,  1392 lines) ── 嵌入资源（勿改）
├── tun/             (8 files,  918 lines) ── TUN 设备管理
├── upgrade/         (1 file,   173 lines) ── 客户端自升级
├── util/           (24 files, 3625 lines) ── 万能工具包 ← 问题！
│   ├── krew/        (5 files,  347 lines) ── Krew 插件发布
│   └── regctl/      (2 files,  214 lines) ── 镜像拷贝进度
│       └── ascii/   (2 files,  142 lines) ── ASCII 进度条
└── webhook/         (5 files,  440 lines) ── Admission webhook
```

## 问题分析

### 问题 1：util/ 是万能工具包（24 files, 3625 lines）

`util/` 混合了完全无关的领域：

| 文件 | 行数 | 领域 | 依赖 |
|------|------|------|------|
| cidr.go | 470 | K8s 集群 CIDR 检测 | K8s clientset |
| pod.go | 366 | Pod 操作（port-forward, exec, wait） | K8s clientset |
| net.go | 258 | TUN 设备查找、ping、ICMP | gopacket, pro-bing |
| kubeconfig.go | 253 | Kubeconfig 文件操作 | clientcmd |
| unstructured.go | 223 | K8s 资源遍历 | cli-runtime |
| kube.go | 221 | K8s API server 操作 | clientcmd |
| docker.go | 220 | Docker 容器生命周期 | docker client |
| util.go | 308 | RolloutStatus, banner, pprof | 混杂 |
| upgrade.go | 166 | GitHub release 下载 | http |
| file.go | 135 | 文件下载、目录映射 | http |
| tls.go | 116 | TLS 证书生成 | crypto |
| helm.go | 102 | Helm release 检测 | helm |
| volume.go | 90 | Docker volume 下载 | docker, cp |
| grpc.go | 132 | gRPC stream 工具 | grpc |
| gvisor.go | 76 | gvisor 协议写入 | gvisor |
| name.go | 53 | 命名工具 | 无 |
| dns.go | 40 | DNS 工具 | miekg/dns |
| exec.go | 57 | 远程执行 | remotecommand |
| port.go | 53 | 端口解析 | 无 |
| version.go | 121 | 版本比较 | go-version |

**最严重的问题**：`util/` 向上依赖了 `pkg/daemon/rpc`（在 grpc.go 中）和 `pkg/driver`（在 util.go 中），违反了工具包应该在依赖图底层的原则。

### 问题 2：目录层级不一致

- `daemon/handler/` 只有 1 个文件（434 行）—— 命名与 `handler/` 混淆
- `driver/` 只有 1 个入口文件 + 2 个子目录
- `syncthing/auto/` 是生成的嵌入资源，应该从目录名看出来

### 问题 3：包名不够描述性

- `cp/` — 不明确，应叫 `filecopy/` 或保持但加 doc
- `run/` — 过于通用
- `core/` — 过于通用，实际是 "network tunnel protocol"

## 整理方案

### 方案原则

1. **不做大规模包迁移** — 迁移 import path 影响所有调用者 + cmd/，风险高
2. **在现有结构内优化** — 加文档、加 doc.go、理清职责
3. **只做明确收益大于风险的改动**

### 具体动作

#### 动作 1：为每个包添加 doc.go（低风险）

每个包目录添加 `doc.go` 文件，说明包的职责和不应该放什么：

```go
// Package core implements the TUN-based network tunnel protocol using gvisor.
// It handles packet routing, TCP/UDP forwarding, and connection pooling between
// the local TUN device and the remote traffic manager.
//
// This package should NOT contain:
// - Kubernetes API operations (use handler/ or util/)
// - DNS configuration (use dns/)
// - Sidecar injection (use inject/)
package core
```

#### 动作 2：重命名 daemon/handler/ 为 daemon/webssh/（低风险）

当前 `daemon/handler/` 只有 `ssh.go` 一个文件，命名与顶层 `handler/` 包混淆。

重命名为 `daemon/webssh/` 更清晰，表明它是 WebSocket SSH 终端处理。

#### 动作 3：给 util/ 的每个文件头部加注释标明归属（最低风险）

在每个 util/ 文件头部加注释，标明这些函数"逻辑上属于哪个领域"，为未来拆分做准备：

```go
// cidr.go contains Kubernetes cluster CIDR detection logic.
// These functions logically belong to a "cluster discovery" package
// but remain in util/ for import compatibility.
```

#### 动作 4：不做的事情

| 不做 | 原因 |
|------|------|
| 拆 util/ 为多个包 | 24 个文件被 15+ 个包引用，迁移成本极高 |
| 合并 tun/ 到 core/ | tun/ 有 8 个平台特定文件，合并会使 core/ 更庞大 |
| 重命名 core/ | 会影响所有 import path |
| 重命名 cp/ | 会影响 cmd/ 和 util/volume.go |
