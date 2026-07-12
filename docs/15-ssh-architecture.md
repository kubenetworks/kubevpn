# SSH Architecture and Lifecycle

## 概述

KubeVPN 的 SSH 子系统提供两大核心能力：

1. **SSH 跳板机（Jump Host）** — 通过 SSH 隧道访问私有网络中的 Kubernetes 集群
2. **SSH 远程终端** — 通过 WebSocket 在远程服务器上创建交互式终端，可选建立 TUN 隧道

## 包结构

```
pkg/ssh/                       SSH 客户端核心库
├── config.go                  SshConfig 类型 + 认证方法 + ~/.ssh/config 解析
├── ssh.go                     连接建立、端口转发、kubeconfig 隧道
├── reverse.go                 反向隧道（SSH -R 等效）
├── scp.go                     SCP 文件传输
├── gssapi.go                  GSSAPI/Kerberos 认证（SPNEGO 协商）
├── gssapi_ccache.go           Kerberos credential cache 解析
├── gssapi_other.go            Kerberos 配置路径（Unix）
├── gssapi_windows.go          Kerberos 配置路径（Windows）
├── filename.go                IP → 文件名转换工具
└── doc.go                     包文档

pkg/daemon/handler/            WebSocket SSH 终端服务
├── ssh.go                     wsHandler — WebSocket 入口、终端会话
├── tunnel.go                  TUN 隧道建立、connWatcher
├── installer.go               远程 kubevpn 安装
└── registry.go                SSH 会话注册表（sync.Map）

pkg/daemon/action/
├── sshdaemon.go               SshStart/SshStop RPC — 远程 TUN 服务端
├── sshconv.go                 RPC SshJump → SshConfig 转换
└── writer.go                  resolveKubeconfig — SSH 隧道 kubeconfig 解析

cmd/kubevpn/cmds/
├── ssh.go                     `kubevpn ssh` 命令
└── sshdaemon.go               `kubevpn ssh-daemon` 命令（隐藏，远程端调用）
```

## 功能一：SSH 跳板机（Jump Host）

### 用途

当 Kubernetes API Server 在私有网络中不可直达时，通过 SSH 隧道访问：

```
┌──────┐    SSH 隧道    ┌──────────┐       ┌──────────────┐
│ 本机  ├──────────────►│ 跳板机    ├──────►│ K8s API 服务器│
└──────┘  127.0.0.1:N   └──────────┘       └──────────────┘
```

### 核心流程

#### 1. 连接建立 — `DialSshRemote(ctx, conf, stopChan)`

`SshConfig` 支持三种入口方式，按优先级：

| 字段 | 入口 | 说明 |
|------|------|------|
| `ConfigAlias` | `AliasRecursion` | 解析 `~/.ssh/config` 中的别名，递归跟踪 ProxyJump 链 |
| `Jump` | `JumpRecursion` | 解析 `--ssh-jump` 参数中嵌套的 SSH 跳板配置 |
| `Addr` | `Dial` | 直接连接到指定地址 |

**ProxyJump 链解析**（`resolveProxyJumpChain`）：

```
~/.ssh/config:
  Host A → ProxyJump B
  Host B → ProxyJump C
  Host C → (终点)

解析结果: [A, B, C]
连接顺序: C → B → A（反向，先连最远的）
```

- 使用 `visited` map 检测循环引用，发现时返回 `"circular ProxyJump detected"` 错误
- 每个跳板的认证信息从 `~/.ssh/config` 读取，回退到命令行参数中的默认值

#### 2. 认证方式 — `GetAuth()`

按优先级尝试：

| 优先级 | 方式 | 配置字段 |
|--------|------|----------|
| 1 | 密码认证 | `Password` |
| 2 | GSSAPI 密码 | `GSSAPIPassword` |
| 3 | GSSAPI Keytab | `GSSAPIKeytabConf` |
| 4 | GSSAPI Cache | `GSSAPICacheFile` |
| 5 | 公钥认证 | `Keyfile`（默认 `~/.ssh/id_rsa`）|

GSSAPI 认证实现了完整的 Kerberos 5 SPNEGO 协商（`Krb5InitiatorClient`），包含状态机：
`InitiatorStart → InitiatorWaitForMutal → InitiatorReady`

#### 3. Kubeconfig 隧道 — `SshJump(ctx, conf, kubeconfigBytes, print)`

完整流程：

```
1. 如果配置了 RemoteKubeconfig：
   a. SSH 到远程服务器
   b. 执行 kubectl/minikube/cat 获取 kubeconfig
   c. 用远程 kubeconfig 替换本地的

2. 选取本地可用端口 N

3. 解析 kubeconfig 中的 API server 地址（如 10.0.1.100:6443）

4. 改写 kubeconfig: API server → 127.0.0.1:N

5. 建立 SSH 端口转发: 127.0.0.1:N → 10.0.1.100:6443
   （通过 PortMapUntil 实现）

6. 将改写后的 kubeconfig 写入临时文件，返回路径
   （ctx 取消时自动删除临时文件）
```

#### 4. 端口转发 — `PortMapUntil(ctx, conf, remote, local)`

实现本地 TCP 端口转发：

- 在本地监听 `local` 端口
- 每个入站连接通过 SSH 通道拨号到 `remote` 地址
- 使用 `sync.Map` 缓存 SSH 客户端连接（`sshClientWrap`），失败时自动重连
- 双向数据复制使用 `copyStream`，任一方向完成或 ctx 取消时关闭两端

#### 5. 与 daemon 的集成

在各 RPC action 中，SSH 跳板通过 `resolveKubeconfig` 统一接入：

```go
// daemon/action/writer.go
func resolveKubeconfig(ctx, jump, kubeconfigBytes, portForward) (string, error) {
    sshConf := parseSshFromRPC(jump)  // RPC SshJump → SshConfig
    if !sshConf.IsEmpty() {
        return ssh.SshJump(ctx, sshConf, kubeconfigBytes, portForward)
    }
    return util.ConvertToTempKubeconfigFile(kubeconfigBytes, "")
}
```

**调用方：**

| RPC | 文件 | 说明 |
|-----|------|------|
| Connect | `connect_elevate.go` | 控制面连接，额外记录 `SshHosts` 用于路由排除 |
| Proxy | `proxy.go` | 代理注入 |
| Sync | `sync.go` | 文件同步 |
| Reset | `reset.go` | 重置工作负载 |
| Disconnect | `disconnect.go` | 断开连接 |
| Uninstall | `uninstall.go` | 卸载 |

**Connect 特殊处理：**

```go
// connect_elevate.go — 用户 daemon 中
if sshConf := parseSshFromRPC(req.SshJump); !sshConf.IsEmpty() {
    connect.SshHosts = sshConf.Host()  // 记录跳板机 IP
    if sshConf.RemoteKubeconfig != "" {
        connect.OriginKubeconfigPath = file  // 标记使用远程 kubeconfig
    }
}
```

`SshHosts` 被添加到 `getAPIServerIPs()` 返回值中，确保 TUN 路由不会覆盖到跳板机的路由（避免 SSH 隧道断裂）。

### 生命周期

```
[用户执行 kubevpn connect --ssh-addr ...]
    │
    ▼
parseSshFromRPC(req.SshJump) → SshConfig
    │
    ▼
resolveKubeconfig(ctx, jump, kubeconfigBytes, portForward)
    │
    ├── SshConfig.IsEmpty() == true → 直接写临时 kubeconfig
    │
    └── SshConfig.IsEmpty() == false:
        │
        ▼
    SshJump(ctx, conf, kubeconfigBytes, print)
        │
        ├── [可选] SSH 远程获取 kubeconfig
        │
        ├── 改写 API server → 127.0.0.1:N
        │
        ├── PortMapUntil(ctx, conf, remote, local)
        │   │
        │   └── 后台: 监听 → 接受连接 → SSH 拨号远端 → 双向复制
        │
        └── 写入临时 kubeconfig 文件
            │
            └── ctx.Done() 时自动删除
```

**销毁：** 当 session context 取消时（断开连接、daemon 退出、用户 ctrl+c），所有资源自动清理：
- SSH 客户端连接关闭（通过 `sshClientWrap.Close()` 调用 cancel + client.Close）
- 本地监听端口关闭
- 临时 kubeconfig 文件删除（`go func() { <-ctx.Done(); os.Remove(path) }()`）

## 功能二：SSH 远程终端（`kubevpn ssh`）

### 用途

在远程 SSH 服务器上打开交互式终端，可选创建 TUN 双向隧道实现本机与远程服务器的 IP 互通。

### 架构

```
┌────────────┐  WebSocket /ws   ┌──────────────┐    SSH     ┌─────────────┐
│ kubevpn ssh├─────────────────►│ 本地 daemon   ├──────────►│ 远程 SSH 服务├
│ (CLI 前端) │                  │ (wsHandler)   │           │ (目标机器)   │
│            │  WebSocket       │               │           │             │
│            │  /resize         │               │           │             │
└────────────┘                  └──────────────┘           └─────────────┘
```

### CLI 端流程（`cmd/kubevpn/cmds/ssh.go`）

```
1. 检查 stdin 是否为终端
2. 获取终端宽高
3. 生成 sessionID (UUID)
4. 构造 Ssh 配置结构体（Config, ExtraCIDR, Width, Height, Platform, SessionID, Lite）
5. JSON 序列化为 WebSocket header "ssh"
6. 连接到本地 daemon 的 Unix socket（GetTCPClient）
7. 创建 WebSocket 连接到 /ws
8. 启动三个并发 goroutine:
   a. monitorSize — 通过 /resize WebSocket 发送终端尺寸变化
   b. stdin → WebSocket（发送用户输入）
   c. WebSocket → stdout（显示远程输出）
      其中穿插 checker: 检测 "Enter terminal <sessionID>" 标记
      收到标记后设置终端为 raw 模式
9. 等待任一 goroutine 完成或 ctx 取消
10. 恢复终端状态
```

### daemon 端流程（`pkg/daemon/handler/`）

#### WebSocket 入口（`ssh.go` init 注册）

**`/ws` 端点：**

```go
http.Handle("/ws", websocket.Handler(func(conn) {
    // 1. 解析 header 中的 SSH 配置
    // 2. 注册 readiness 信号: sessionRegistry.storeReady(sessionID, ctx)
    // 3. 创建 wsHandler
    // 4. 执行 handle(lite)
    // 5. defer: sessionRegistry.cleanup(sessionID)
}))
```

**`/resize` 端点：**

```go
http.Handle("/resize", websocket.Handler(func(conn) {
    // 1. 等待 session ready（通过 readyCtx.Done()）
    // 2. 从 registry 获取 SSH session
    // 3. 循环读取 TerminalSize JSON
    // 4. 调用 session.WindowChange(height, width)
}))
```

#### 会话注册表（`registry.go`）

```go
var sessionRegistry = &registry{
    sessions: sync.Map{},  // map[sessionID] → *ssh.Session
    ready:    sync.Map{},  // map[sessionID] → context.Context
}
```

**生命周期：**

| 时机 | 操作 |
|------|------|
| WebSocket 连接建立 | `storeReady(id, ctx)` — 注册 readiness context |
| SSH 终端就绪 | `storeSession(id, session)` — 存储 SSH session；`condReady()` — 取消 readiness context 通知 /resize 端 |
| WebSocket 连接关闭 | `cleanup(id)` — 删除 sessions 和 ready 条目 |

#### wsHandler.handle(lite) 流程

```
handle(lite):
    │
    ├── DialSshRemote(ctx, sshConfig) → ssh.Client
    │
    ├── [lite == false] createTunnel(ctx, cli):
    │   │
    │   ├── installKubevpnOnRemote(ctx, cli)
    │   │   │
    │   │   ├── 检查远程是否已有 kubevpn 命令
    │   │   ├── [没有] 下载最新版本 → SCP 到远程
    │   │   └── 启动远程 daemon（kubevpn status）
    │   │
    │   ├── 获取本机 IP
    │   ├── PortMapUntil: 127.0.0.1:localPort → 127.0.0.1:10801 (远程)
    │   ├── RemoteRun: kubevpn ssh-daemon --client-ip <本机IP>
    │   │   └── 远程 daemon 执行 SshStart RPC
    │   │       ├── 创建 TUN 设备（net=198.18.0.0/32 路由 IP）
    │   │       ├── 启动 gvisor TCP 监听(:10801)
    │   │       └── 添加客户端 IP 路由到 TUN
    │   │
    │   ├── 本地创建 TUN 设备 + gvisor 协议栈
    │   │   └── 路由: 客户端 IP → forward tcp://127.0.0.1:localPort
    │   │
    │   └── 启动心跳 ping (每 15 秒)
    │
    ├── connWatcher 包装 WebSocket（检测断开）
    │
    └── terminal(ctx, cli, rw):
        │
        ├── NewSession → 绑定 stdin/stdout/stderr 到 WebSocket
        ├── sessionRegistry.storeSession(id, session)
        ├── condReady() — 通知 /resize 端可以开始
        ├── RequestPty("xterm-256color", height, width, modes)
        ├── Shell()
        └── session.Wait() — 阻塞直到 shell 退出
```

### 两种模式

| 模式 | `--lite` | 行为 |
|------|----------|------|
| Full | false | 安装 kubevpn + 创建 TUN 隧道 + 打开终端 |
| Lite | true | 仅打开终端（纯 SSH 终端） |

**Full 模式网络拓扑：**

```
┌─────────┐  TUN   ┌──────────────┐  SSH 端口转发  ┌─────────┐  TUN   ┌──────────┐
│ 本机进程 ├───────►│ 本地 gvisor  ├──────────────►│远程gvisor├───────►│远程服务  │
│ 198.18.x│        │ (tcp forward)│               │ (:10801) │        │          │
└─────────┘        └──────────────┘               └─────────┘        └──────────┘
```

### 连接监控 — connWatcher

`connWatcher` 包装 WebSocket 连接，在任意 Read/Write 错误时通过 channel 通知：

```go
type connWatcher struct {
    ch chan struct{}
    sync.Once  // 确保 channel 只关闭一次
    net.Conn
}
```

当 WebSocket 断开时，`closed()` channel 被关闭，触发 context cancel，级联清理整个会话。

## 功能三：SSH 远程 TUN 服务端（`ssh-daemon`）

### RPC 定义

```protobuf
rpc SshStart (SshStartRequest) returns (SshStartResponse) {}
rpc SshStop (SshStopRequest) returns (SshStopResponse) {}

message SshStartRequest { string ClientIP = 1; }
message SshStartResponse { string ServerIP = 1; }
message SshStopRequest { string ClientIP = 1; }
message SshStopResponse { string ServerIP = 1; }
```

### SshStart 逻辑（`daemon/action/sshdaemon.go`）

```
SshStart(ctx, req):
    │
    ├── Lock（互斥，防止并发创建）
    │
    ├── 解析 clientIP CIDR
    │
    ├── [sshServerIP == ""] 首次启动:
    │   │
    │   ├── 创建 TUN + gvisor 节点:
    │   │   ├── tun: net=198.18.0.0/32（默认服务端 IP）
    │   │   └── gtcp: :10801（gvisor TCP 监听）
    │   │
    │   ├── 启动 handler.Run(sshCtx, servers)
    │   │
    │   └── 记录 sshServerIP, sshCancelFunc
    │
    ├── 查找 TUN 设备
    │
    ├── 添加路由: clientIP → TUN 设备
    │
    └── 返回 sshServerIP
```

### SshStop 逻辑

```go
func (svr *Server) SshStop(ctx, req) {
    if svr.sshCancelFunc != nil {
        svr.sshCancelFunc()  // 关闭 TUN + gvisor 服务
    }
}
```

### Server 状态字段

```go
type Server struct {
    // ...
    sshServerIP   string             // 当前 SSH TUN 服务端 IP
    sshCancelFunc context.CancelFunc // 用于关闭 SSH TUN 服务
}
```

**多客户端支持：** TUN 设备只创建一次（惰性初始化），后续客户端只添加路由。这意味着多个 `kubevpn ssh` 客户端可以共享同一个 TUN 设备。

## 生命周期总结

### SSH 跳板机生命周期

```
创建：                                        销毁：
  用户 RPC 请求                                session context 取消
    → parseSshFromRPC                            → SSH 客户端关闭
    → resolveKubeconfig                          → 本地监听端口关闭
      → SshJump                                  → 临时 kubeconfig 删除
        → DialSshRemote（创建 SSH 连接）
        → PortMapUntil（创建本地监听 + 端口转发）
        → 写入临时 kubeconfig

生存期 = session context 的生存期
```

### SSH 远程终端生命周期

```
创建（Full 模式）：                             销毁：
  CLI 连接 WebSocket /ws                        任一条件触发:
    → DialSshRemote                             ├── WebSocket 断开(connWatcher)
    → installKubevpnOnRemote                    ├── SSH shell 退出(session.Wait)
    → 建立 SSH 端口转发                          ├── 用户 ctrl+c (ctx cancel)
    → RemoteRun ssh-daemon                      └── daemon 退出
      → 远端 SshStart RPC                      销毁级联:
        → 创建 TUN + gvisor                       → cancel ctx
    → 本地创建 TUN + gvisor                       → SSH session 关闭
    → 启动心跳 ping                               → SSH client 关闭
    → 打开 SSH 终端                               → 本地 TUN 停止
    → 注册到 sessionRegistry                      → sessionRegistry.cleanup
                                                  → 终端状态恢复
                                                [远端 TUN 需要单独 SshStop]
```

### SSH TUN 服务端生命周期

```
创建：                                        销毁：
  SshStart RPC                                SshStop RPC
    → [惰性] 创建 TUN + gvisor                   → sshCancelFunc()
    → 添加客户端路由                               → TUN + gvisor 关闭

生存期: 从第一个客户端 SshStart 到显式 SshStop
注意: daemon 退出时 sshCancelFunc 不会被自动调用，
      但进程退出会导致 TUN 设备被内核回收。
```

## Proto 定义

`SshJump` 消息被嵌入到以下 RPC 请求中：

| 请求消息 | 字段编号 |
|----------|----------|
| ConnectRequest | field 5 |
| DisconnectRequest | field 5 |
| ProxyRequest | field 9 |
| SyncRequest | field 8 |
| ResetRequest | field 4 |
| UninstallRequest | field 3 |

```protobuf
message SshJump {
  string Addr = 1;
  string User = 2;
  string Password = 3;
  string Keyfile = 4;
  string Jump = 5;           // 嵌套跳板配置
  string ConfigAlias = 6;    // ~/.ssh/config 别名
  string RemoteKubeconfig = 7;
  string GSSAPIKeytabConf = 8;
  string GSSAPIPassword = 9;
  string GSSAPICacheFile = 10;
}
```

## 相关设计文档

- `02-dual-daemon.md` — 双 daemon 模型（resolveKubeconfig 在用户 daemon 中运行）
- `12-session-lifecycle.md` — SessionLifecycle（管理 SSH 隧道临时文件的清理）
- `14-rpc-daemon-mapping.md` — RPC 到 daemon 的映射
