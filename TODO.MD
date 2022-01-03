## TODO

- [x] 访问集群网络
- [x] 域名解析功能
- [x] 支持多个 service 反向代理
- [x] 短域名解析
- [ ] 优化 DHCP 功能
- [x] 支持多种类型，例如 statefulset, replicaset...
- [ ] 支持 ipv6
- [ ] 自己实现 socks5 协议
- [ ] 考虑是否需要把 openvpn tap/tun 驱动作为后备方案
- [x] 加入 TLS 以提高安全性
- [ ] 写个 CNI 网络插件，直接提供 VPN 功能
- [ ] 优化重连逻辑
- [x] 支持 service mesh
- [ ] service mesh 支持多端口
- [ ] 使用自己写的 proxy 替换 envoy
- [ ] 优化性能，Windows 上考虑使用 IPC 通信
- [ ] 自己写个 control plane
- [ ] 考虑是否将 control plane 和服务分开
- [ ] 写单元测试，优化 GitHub action
- [ ] Linux 和 macOS 也改用 WireGuard library
- [ ] 探测是否有重复路由的 utun设备，禁用 `sudo ifconfig utun1 down`

