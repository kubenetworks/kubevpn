# Sidecar TUN IP 热更新 — 方案 A 详细设计（v3: Control Plane 配置中心模式）

## 1. 问题背景

当前 sidecar TUN IP 由 MutatingWebhook 在 Pod 创建时一次性分配，之后不可变更。

## 2. 设计目标

- **利用现有 envoy control-plane 基础设施作为配置中心**分发 TUN IP
- Sidecar 启动时向 control-plane 请求 IP（传入 OwnerID）
- Control-plane 检测到 IP 变化时推送新配置给 sidecar
- Sidecar 定时轮询配置作为兜底
- 去掉 webhook 的 IP 分配职责

## 3. 核心思路

现有 envoy control-plane（`pkg/controlplane`）已经实现了：
- gRPC server（xDS 协议，端口 9002）
- ConfigMap watcher — 监听 `kubevpn-traffic-manager` ConfigMap 变化
- Processor — 将 ConfigMap 中的 Virtual/Rule 配置转换为 envoy snapshot

**扩展点：** 在 control-plane 的 gRPC server 上新增一个 **TUN IP 配置服务**。Sidecar 通过同一个 gRPC 连接获取和监听自己的 TUN IP 配置。

```
                   Traffic Manager Pod
                   ┌────────────────────────────────┐
                   │  envoy control-plane (gRPC:9002)│
                   │  ├─ xDS (envoy 配置)            │
                   │  └─ TunConfig service (NEW)    │
                   │      ├─ GetTunIP(ownerID)      │
                   │      └─ WatchTunIP(ownerID)    │
                   │                                │
                   │  ConfigMap watcher              │
                   │  ├─ KeyEnvoy → envoy snapshot  │
                   │  └─ KeyDHCP → TUN IP 分配     │
                   └────────────────────────────────┘
                          ↑ gRPC
                          │
              ┌───────────┴──────────┐
              │  Sidecar (workload)   │
              │  kubevpn server       │
              │  ├─ 启动时: GetTunIP  │
              │  ├─ 后台: WatchTunIP  │
              │  └─ IP 变化 → 热更新  │
              └──────────────────────┘
```

## 4. 组件设计

### 4.1 新增 gRPC 服务定义

在已有的 control-plane gRPC server 上注册新服务（不需要新端口）：

```protobuf
// pkg/daemon/rpc/daemon.proto (新增)

service TunConfigService {
  // GetTunIP 分配或获取当前 TUN IP。
  // ownerID 标识调用者身份（sidecar 使用 podName 或 connectionID）。
  // 如果该 ownerID 已有分配，返回现有 IP；否则 DHCP 分配新 IP。
  rpc GetTunIP(TunIPRequest) returns (TunIPResponse);
  
  // WatchTunIP 长连接流，当 ownerID 的 IP 发生变化时推送新配置。
  rpc WatchTunIP(TunIPRequest) returns (stream TunIPResponse);
}

message TunIPRequest {
  string OwnerID = 1;        // sidecar 身份标识 (podName or nodeID)
  string Namespace = 2;      // workload namespace
}

message TunIPResponse {
  string IPv4 = 1;           // e.g. "198.18.0.5/32"
  string IPv6 = 2;           // e.g. "fd11::5/128"
  int64  Version = 3;        // 配置版本号，用于变更检测
}
```

### 4.2 Control-Plane 端实现

在 `pkg/controlplane/` 中新增 TUN IP 管理逻辑：

```go
// pkg/controlplane/tun_config.go (新文件)

type TunConfigServer struct {
    rpc.UnimplementedTunConfigServiceServer
    
    dhcp      *dhcp.Manager
    mu        sync.RWMutex
    allocated map[string]*allocation  // ownerID → IP allocation
    watchers  map[string][]chan *rpc.TunIPResponse
}

type allocation struct {
    IPv4    *net.IPNet
    IPv6    *net.IPNet
    Version int64
}

// GetTunIP: 如果 ownerID 已有分配返回现有 IP，否则 DHCP rent 新 IP
func (s *TunConfigServer) GetTunIP(ctx context.Context, req *rpc.TunIPRequest) (*rpc.TunIPResponse, error) {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    if alloc, ok := s.allocated[req.OwnerID]; ok {
        return &rpc.TunIPResponse{
            IPv4: alloc.IPv4.String(),
            IPv6: alloc.IPv6.String(),
            Version: alloc.Version,
        }, nil
    }
    
    // 新分配
    v4, v6, err := s.dhcp.RentIP(ctx)
    if err != nil {
        return nil, err
    }
    alloc := &allocation{IPv4: v4, IPv6: v6, Version: time.Now().UnixNano()}
    s.allocated[req.OwnerID] = alloc
    
    return &rpc.TunIPResponse{
        IPv4: v4.String(), IPv6: v6.String(), Version: alloc.Version,
    }, nil
}

// WatchTunIP: 长连接，IP 变化时推送
func (s *TunConfigServer) WatchTunIP(req *rpc.TunIPRequest, stream rpc.TunConfigService_WatchTunIPServer) error {
    ch := make(chan *rpc.TunIPResponse, 1)
    s.addWatcher(req.OwnerID, ch)
    defer s.removeWatcher(req.OwnerID, ch)
    
    for {
        select {
        case resp := <-ch:
            if err := stream.Send(resp); err != nil {
                return err
            }
        case <-stream.Context().Done():
            return nil
        }
    }
}

// NotifyIPChange: 当 ConfigMap watcher 检测到 DHCP 数据变化时调用
func (s *TunConfigServer) NotifyIPChange(ownerID string, newIPv4, newIPv6 *net.IPNet) {
    s.mu.Lock()
    alloc := s.allocated[ownerID]
    if alloc != nil {
        alloc.IPv4 = newIPv4
        alloc.IPv6 = newIPv6
        alloc.Version = time.Now().UnixNano()
    }
    s.mu.Unlock()
    
    // 推送给所有 watcher
    s.notifyWatchers(ownerID, &rpc.TunIPResponse{
        IPv4: newIPv4.String(), IPv6: newIPv6.String(), Version: alloc.Version,
    })
}
```

### 4.3 在 control-plane gRPC server 注册新服务

修改 `pkg/controlplane/server.go`：
```go
func runServer(ctx context.Context, server serverv3.Server, tunConfig *TunConfigServer, port uint) error {
    // ... existing setup ...
    
    // 注册 envoy xDS 服务（已有）
    discoverygrpc.RegisterAggregatedDiscoveryServiceServer(grpcServer, server)
    // ...
    
    // 注册 TUN IP 配置服务（新增）
    rpc.RegisterTunConfigServiceServer(grpcServer, tunConfig)
    
    return grpcServer.Serve(listener)
}
```

### 4.4 Sidecar 端：启动时获取 IP + 后台 Watch

修改 `kubevpn server` 的 TUN 协议工厂（`tunProtocolFactory`）：

```go
func tunProtocolFactory(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
    netAddr := node.Get("net")
    
    if netAddr == "" {
        // 新路径：从 control-plane 获取 IP
        ownerID := os.Getenv(config.EnvPodName) // pod name 作为 ownerID
        trafficManagerAddr := os.Getenv("TrafficManagerService")
        
        // gRPC 连接到 traffic manager 的 9002 端口
        conn, _ := grpc.Dial(trafficManagerAddr+":9002", grpc.WithInsecure())
        client := rpc.NewTunConfigServiceClient(conn)
        
        // 获取 IP
        resp, err := client.GetTunIP(ctx, &rpc.TunIPRequest{
            OwnerID:   ownerID,
            Namespace: os.Getenv(config.EnvPodNamespace),
        })
        if err != nil {
            return nil, nil, fmt.Errorf("get TUN IP from control-plane: %w", err)
        }
        netAddr = resp.IPv4
        net6Addr = resp.IPv6
        
        // 后台启动 WatchTunIP（在 TUN 创建后）
        go watchAndUpdateTunIP(ctx, client, ownerID, tunDevice)
    }
    
    // 创建 TUN（使用获取到的 IP）
    listener, err := tun.Listener(tun.Config{Addr: netAddr, ...})
    ...
}
```

### 4.5 Sidecar Watch + 定时轮询兜底

```go
func watchAndUpdateTunIP(ctx context.Context, client rpc.TunConfigServiceClient, ownerID string, tunName string) {
    currentVersion := int64(0)
    
    for ctx.Err() == nil {
        // 尝试 Watch 流
        stream, err := client.WatchTunIP(ctx, &rpc.TunIPRequest{OwnerID: ownerID})
        if err != nil {
            // Watch 失败，降级为定时轮询
            pollTunIP(ctx, client, ownerID, tunName, &currentVersion)
            continue
        }
        
        for {
            resp, err := stream.Recv()
            if err != nil {
                break // 重连
            }
            if resp.Version != currentVersion {
                applyIPChange(tunName, resp)
                currentVersion = resp.Version
            }
        }
    }
}

// 定时轮询兜底（Watch 断连时）
func pollTunIP(ctx context.Context, client rpc.TunConfigServiceClient, ownerID, tunName string, version *int64) {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            resp, err := client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: ownerID})
            if err == nil && resp.Version != *version {
                applyIPChange(tunName, resp)
                *version = resp.Version
            }
        case <-ctx.Done():
            return
        }
    }
}

func applyIPChange(tunName string, resp *rpc.TunIPResponse) {
    oldIPv4, oldIPv6, _, _ := util.GetTunDeviceIP(tunName)
    
    // 1. 更新 TUN 设备 IP
    if resp.IPv4 != "" {
        tun.ChangeIP(tunName, formatCIDR(oldIPv4), resp.IPv4)
    }
    if resp.IPv6 != "" {
        tun.ChangeIP(tunName, formatCIDR(oldIPv6), resp.IPv6)
    }
    
    // 2. 更新 iptables（VPN-only 模式）
    tun.UpdateDNAT(oldIPv4, parseIP(resp.IPv4))
    
    // 3. heartbeat 下次 tick 自动使用新 IP（从 OS 重读）
}
```

### 4.6 `pkg/tun` — ChangeIP + UpdateDNAT

同之前设计（不变）。

### 4.7 heartbeat 动态读取 IP

同之前设计：每次 tick 从 OS 重读 `util.GetTunDeviceIP(tunIfi.Name)`。

### 4.8 去掉 Webhook IP 分配

`pkg/webhook/pods.go` handleCreate：移除 DHCP + patch env 逻辑。
handleDelete：保留 ReleaseIP 作为兜底。

## 5. ConfigMap Watcher 集成

现有的 control-plane watcher 已经监听 ConfigMap 变化。扩展它来检测 DHCP 字段变化：

```go
// pkg/controlplane/watcher.go — 修改 notifyCh 发送逻辑

// 原来只发 KeyEnvoy 变化:
notifyCh <- NotifyMessage{Content: configMap.Data[config.KeyEnvoy]}

// 新增: 检测 KeyDHCP 变化，通知 TunConfigServer
if dhcpChanged(old, new) {
    tunConfigServer.ReconcileDHCP(configMap.Data[config.KeyDHCP])
}
```

`ReconcileDHCP` 解析 DHCP 数据，检查每个已注册 ownerID 的 IP 是否仍有效，无效则重新分配并推送。

## 6. 完整生命周期

```
Pod 创建 (有 sidecar, 无 webhook IP 分配)
  ↓
Sidecar 容器启动 (kubevpn server -l "tun:/tcp://tm:10801?net=&route=...")
  → gRPC dial traffic-manager:9002
  → GetTunIP(ownerID=podName) → 198.18.0.5/32
  → createTun("utun0", "198.18.0.5/32")
  → iptables DNAT → 198.18.0.5
  → 连接 traffic-manager:10801 (数据面 TCP)
  → heartbeat(198.18.0.5)
  → 后台: WatchTunIP(ownerID) stream
  │
  ├── IP 变更场景 ──────────────────────────────┐
  │   外部 client: 发现 IP 冲突                    │
  │   → 调用 TunConfigServer.NotifyIPChange      │
  │      (释放旧 IP, 分配新 IP, 更新 ConfigMap)   │
  │   → WatchTunIP stream 推送 新 IP              │
  │                                              │
  │   Sidecar 收到推送:                           │
  │   → tun.ChangeIP("utun0", old, new)          │
  │   → iptables update                         │
  │   → 下次 heartbeat 用新 IP                   │
  │   → Server RouteHub auto-register           │
  │                                              │
  ├── Watch 断连 → 降级为 30s 轮询 GetTunIP       │
  │   → 发现 version 变了 → 同上热更新流程         │
  │                                              │
  └── Pod 删除 ──────────────────────────────────┘
      → graceful shutdown
      → TunConfigServer 检测到连接断开
      → 释放 IP 回 DHCP 池
```

## 7. 优势对比

| | Webhook 模式（当前） | ConfigMap Watch 模式（v2） | Control-Plane 模式（本方案） |
|---|---|---|---|
| IP 分配时机 | Pod 创建时 | Sidecar 启动时 | Sidecar 启动时 |
| IP 变更 | 需重启 Pod | 需解析 ConfigMap DHCP 格式 | gRPC 推送，类型安全 |
| K8s RBAC 需求 | Webhook 需要高权限 | Sidecar 需 ConfigMap 读写权限 | Sidecar 只需 gRPC 连接（无 K8s API） |
| 延迟 | 即时（Pod 创建时） | ConfigMap informer 延迟 | gRPC stream 实时推送 |
| 复杂度 | Webhook + patch | ConfigMap 解析 | gRPC 服务（复用已有基础设施） |
| 兜底机制 | 无 | informer 重连 | 定时轮询 GetTunIP |

## 8. 文件变更清单

| 文件 | 操作 | 说明 |
|------|------|------|
| `pkg/daemon/rpc/daemon.proto` | 修改 | 新增 TunConfigService |
| `pkg/daemon/rpc/*.pb.go` | 重新生成 | `make gen` |
| `pkg/controlplane/tun_config.go` | 新建 | TunConfigServer 实现 |
| `pkg/controlplane/server.go` | 修改 | 注册 TunConfigService |
| `pkg/controlplane/controlplane.go` | 修改 | 创建并传递 TunConfigServer |
| `pkg/controlplane/watcher.go` | 修改 | DHCP 变化通知 TunConfigServer |
| `pkg/tun/ip.go` | 新建 | ChangeIP 签名 |
| `pkg/tun/ip_linux.go` | 新建 | Linux 实现 |
| `pkg/tun/ip_darwin.go` | 新建 | macOS 实现 |
| `pkg/tun/ip_windows.go` | 新建 | Windows stub |
| `pkg/tun/iptables_linux.go` | 新建 | DNAT 更新 |
| `pkg/core/tun_client.go` | 修改 | heartbeat 动态读 IP |
| `pkg/core/protocol_registry.go` | 修改 | tunProtocolFactory 支持从 control-plane 获取 IP |
| `pkg/inject/container.go` | 修改 | 移除 TunIPv4/v6 env，net= 参数置空 |
| `pkg/webhook/pods.go` | 修改 | handleCreate 移除 DHCP/patch |

## 9. 兼容性

- `?net=` 非空 → 旧路径（直接用 env 中的 IP）
- `?net=` 空 → 新路径（从 control-plane GetTunIP）
- 新旧 sidecar 可共存在同一集群

## 10. 验证方式

1. `go build ./...`
2. `make gen` — 生成 protobuf
3. `go test ./pkg/controlplane/... ./pkg/tun/... ./pkg/core/...`
4. 集成测试:
   - 启动 traffic manager（含 TunConfigService）
   - 启动 sidecar（net= 空）
   - 验证 sidecar 通过 gRPC 获取 IP 并创建 TUN
   - 调用 NotifyIPChange 模拟 IP 变更
   - 验证 sidecar 收到推送并热更新 TUN
   - 断开 Watch stream，验证轮询兜底生效
