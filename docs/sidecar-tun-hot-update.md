# Sidecar TUN 设备热更新 IP 分析

## 当前流程

```
Pod 创建
  ↓
MutatingWebhook (pkg/webhook/pods.go)
  → DHCP rent IP (v4 + v6)
  → Patch env: TunIPv4="198.18.0.5/32", TunIPv6="fd11::5/128"
  ↓
Sidecar 容器启动
  → iptables DNAT: PREROUTING → ${TunIPv4}
  → kubevpn server -l "tun:/tcp://traffic-manager:10801?net=${TunIPv4}&net6=${TunIPv6}&route=${CIDR4}"
  ↓
tunProtocolFactory (pkg/core/protocol_registry.go)
  → tun.Listener(Config{Addr: "198.18.0.5/32", Routes: ...})
  → createTun(cfg) → OS: 创建 utun*, 设置 IP, 添加路由, link up
  ↓
TunHandler (pkg/core/tun_server.go)
  → HandleClient: 建立到 traffic-manager 的 TCP 连接
  → heartbeats(): 读取 TUN 设备 IP, 发送 ICMP 到 RouterIP
  → Server 端 RouteHub.AddRoute(srcIP=198.18.0.5, conn)
```

**TUN IP 在三个地方硬编码：**
1. OS 层 TUN 接口地址 (`netlink.NetworkLinkAddIp`)
2. iptables DNAT 规则 (`-j DNAT --to 198.18.0.5`)
3. heartbeat 包的 src IP（从 TUN 设备读取）

## 为什么当前不支持热更新

修改 env var 需要 pod restart（K8s 规范）。当前只有一个场景会改变 IP：
- Pod 被重新调度 → webhook 重新 rent IP → 全新 sidecar 启动

## 热更新设计方案

### 方案 A：Sidecar 监听 ConfigMap 变更

```
Sidecar 内部：
1. watch DHCP ConfigMap
2. 检测到 "我的 IP 被换了"
3. 执行 IP 切换:
   a. netlink.NetworkLinkDelIp(ifc, oldIP)
   b. netlink.NetworkLinkAddIp(ifc, newIP)
   c. iptables -t nat -R PREROUTING ... -j DNAT --to ${newIP}
   d. 路由不需要改（TUN 设备未变，路由基于设备 index）
4. 下一次 heartbeat 自动使用新 IP
   → Server RouteHub 自动注册新映射
```

**优点：** 无需外部触发，sidecar 自治
**缺点：** 需要在 sidecar 内嵌入 ConfigMap watcher；增加复杂度

### 方案 B：通过 gRPC/HTTP 端点触发

```
外部触发（client 或 controller）：
1. Client 调用 sidecar 暴露的 gRPC 端点: UpdateTunIP(newIPv4, newIPv6)
2. Sidecar 执行上述 IP 切换步骤
```

**优点：** 精确控制触发时机
**缺点：** 需要新增 RPC 接口；需要 client 能访问 sidecar（已有 port-forward）

### 方案 C：信号触发 + ConfigMap 存储新 IP

```
1. 将新 IP 写入 ConfigMap 的某个 key（如 pod-name.newIP）
2. 发送 SIGUSR1 给 sidecar 进程
3. Sidecar 收到信号 → 读 ConfigMap 获取新 IP → 执行切换
```

**优点：** 简单，无需 watcher 长连接
**缺点：** 需要能 exec 进 pod 发信号（或用 K8s lifecycle hooks）

## 技术可行性验证

### TUN 设备是否支持不重建修改 IP？

**Linux:**
```go
ifc, _ := net.InterfaceByName("utun0")
netlink.NetworkLinkDelIp(ifc, oldIP, oldCIDR)  // 删除旧 IP
netlink.NetworkLinkAddIp(ifc, newIP, newCIDR)  // 添加新 IP
// TUN fd 继续有效，Read/Write 不受影响
```

**验证：** TUN 是内核虚拟设备，IP 地址是 OS 网络栈的元数据，不影响 fd 读写。修改 IP 后：
- 已有的 fd (tunConn.ifce) **继续工作**
- 路由表基于设备 index，**不受 IP 变更影响**
- 内核不会因 IP 变更关闭 TUN fd

### iptables DNAT 是否可以原子切换？

```bash
# 方案1：replace 规则
iptables -t nat -R PREROUTING 1 ! -p icmp -j DNAT --to ${newIP}

# 方案2：先加后删（无中断）
iptables -t nat -I PREROUTING 1 ! -p icmp -j DNAT --to ${newIP}
iptables -t nat -D PREROUTING 2
```

两种方式都可以做到零丢包。

### heartbeat 是否自动适应新 IP？

当前 heartbeat 在启动时从 TUN 设备读取 IP：
```go
srcIPv4, srcIPv6, _, err := util.GetTunDeviceIP(tunIfi.Name)
```

**问题：** 这个值被缓存了（只读一次）。IP 变更后 heartbeat 仍发送旧 IP。

**修复：** 让 heartbeat 每次发送前重新读取设备 IP，或通过 channel 通知 IP 变更。

### Server 端 RouteHub 是否自动适应？

```go
func (hub *RouteHub) AddRoute(ctx, srcIP, conn) {
    hub.RouteMapTCP.LoadOrStore(string(srcIP), &ConnList{...})
}
```

**是的。** 当新 IP 的第一个包到达 server，`AddRoute` 自动注册 `newIP → conn` 映射。旧 IP 映射保留但无害（同一 conn）。

## 推荐实现方案

推荐 **方案 A 简化版**：不 watch ConfigMap，而是在 `kubevpn server` 中添加 IP 刷新能力：

### 修改 pkg/core/tun_client.go

```go
type ClientDevice struct {
    tunDevice
    reconnected chan struct{}
    ipChanged   chan [2]*net.IPNet  // NEW: 通知 IP 变更
    slots       []chan *Packet
}

func (d *ClientDevice) heartbeats(ctx context.Context) {
    tunIfi, _ := util.GetTunDeviceByConn(d.tun)
    srcIPv4, srcIPv6, dockerSrcIPv4, _ := util.GetTunDeviceIP(tunIfi.Name)
    
    // ...
    for ctx.Err() == nil {
        select {
        case <-ticker.C:
            sendAll("periodic")
        case <-d.reconnected:
            sendAll("reconnected")
        case newIPs := <-d.ipChanged:      // NEW
            srcIPv4 = newIPs[0].IP
            srcIPv6 = newIPs[1].IP
            sendAll("ip-changed")
        case <-ctx.Done():
            return
        }
    }
}
```

### 修改 pkg/tun/tun_linux.go — 添加 ChangeIP

```go
func ChangeIP(ifName string, oldAddr, newAddr string) error {
    ifc, err := net.InterfaceByName(ifName)
    if err != nil {
        return err
    }
    
    // Remove old IP
    if oldAddr != "" {
        oldIP, oldCIDR, _ := net.ParseCIDR(oldAddr)
        _ = netlink.NetworkLinkDelIp(ifc, oldIP, oldCIDR)
    }
    
    // Add new IP
    newIP, newCIDR, err := net.ParseCIDR(newAddr)
    if err != nil {
        return err
    }
    return netlink.NetworkLinkAddIp(ifc, newIP, newCIDR)
}
```

### 修改 container.go — iptables 切换脚本

将 iptables 规则改为可更新模式：
```bash
# 启动时保存规则编号
iptables -t nat -A PREROUTING ! -p icmp -j DNAT --to ${TunIPv4}

# 更新时
update_tun_ip() {
    iptables -t nat -R PREROUTING 1 ! -p icmp -j DNAT --to $1
}
```

## 整体改动量评估

| 组件 | 修改量 | 说明 |
|------|--------|------|
| `pkg/tun/tun_linux.go` | 新增 `ChangeIP` 函数 (~15 行) | OS 层 IP 修改 |
| `pkg/tun/tun_darwin.go` | 新增 `ChangeIP` 函数 (~15 行) | macOS 版本 |
| `pkg/core/tun_client.go` | 修改 heartbeats (~10 行) | 支持 IP 刷新通知 |
| `cmd/kubevpn/cmds/server.go` | 新增信号处理 (~20 行) | SIGUSR1 触发 IP 重读 |
| `pkg/inject/container.go` | iptables 命令可更新化 | 改为函数可调用 |
| Server 端 | **无需修改** | RouteHub 天然支持 |

## 触发场景

什么时候需要热更新 sidecar TUN IP：
1. **DHCP 租约冲突** — 另一个 client 抢占了相同 IP
2. **IP 池扩容** — 管理员修改了 CIDR 范围，旧 IP 不再有效
3. **连接恢复** — client 断连后重连获得新 IP，sidecar 需要同步更新
4. **多 client 切换** — 不同 client 轮流代理同一个 workload

## 结论

Sidecar TUN 设备**完全可以支持热更新 IP**，核心改动集中在 client 侧（sidecar 内）：
- TUN fd 不需要重建（只改 OS 元数据）
- iptables 可以原子替换
- heartbeat 需要支持 IP 刷新通知
- Server 端 RouteHub 天然支持（每包注册）
- 改动量约 60-80 行新增代码
