# Local Proxy Design For Nested VPNs

## Summary

`proxy-out` adds a local outbound proxy mode for environments where `kubevpn connect` succeeds but operating-system route ownership is controlled by another VPN client. Instead of relying on cluster CIDR routing, client applications send TCP traffic to a local proxy, and kubevpn forwards that traffic into the cluster through the Kubernetes API.

This version ships:

- a standalone `kubevpn proxy-out` command
- a managed SOCKS5 mode via `kubevpn connect --socks`
- SOCKS5 and optional HTTP CONNECT listeners
- remote resolution of cluster Service names when clients use `socks5h`
- support for Service ClusterIP and Pod IP TCP targets

This version does not ship:

- PAC files
- a DNS listener
- transparent interception
- `kubevpn status` integration for proxy listener state

## Problem

In nested VPN setups, another VPN client may own the host route table or DNS path. That breaks kubevpn's normal route-based access even when the tunnel itself is healthy. The most common symptoms are:

- cluster IP traffic leaves through the outer tunnel instead of kubevpn
- split DNS settings do not resolve cluster names end to end
- local applications cannot reliably reach Services or Pod IPs without manual proxy support

## Implemented Design

### Data path

For outbound proxy mode, the user-facing API is a local listener:

```text
local app
  -> SOCKS5 or HTTP CONNECT
  -> kubevpn proxy-out server
  -> Kubernetes API port-forward to a resolved Pod endpoint
  -> cluster destination
```

### Name and target resolution

- Service hostnames such as `svc`, `svc.ns`, `svc.ns.svc.cluster.local` are resolved inside kubevpn
- Service ClusterIP targets are mapped back to a Service, then to a ready endpoint Pod
- Pod IP targets dial the matching running Pod directly
- short names use the active kubeconfig namespace as the default namespace

### Lifecycle

- `kubevpn proxy-out` runs as a foreground local proxy process
- `kubevpn connect --socks` starts a managed background SOCKS5 proxy bound to the connection ID
- `kubevpn disconnect <connection-id>`, `kubevpn disconnect --all`, and `kubevpn quit` stop managed proxies
- managed state is stored under the kubevpn local state directory; transient kubeconfig files are deleted on stop
- proxy logs are intentionally retained for troubleshooting

## User Experience

### Standalone mode

```bash
kubevpn proxy-out --listen-socks 127.0.0.1:1080
curl --proxy socks5h://127.0.0.1:1080 http://productpage.default.svc.cluster.local:9080
```

### Managed mode

```bash
kubevpn connect --socks
curl --proxy socks5h://127.0.0.1:1080 http://productpage.default.svc.cluster.local:9080
```

### Notes

- default SOCKS5 listen address is `127.0.0.1:1080`
- HTTP CONNECT is optional and disabled by default
- proxy mode handles TCP traffic only in this version
- use `socks5h` when you want proxy-side cluster DNS resolution

## Implementation Notes

- Command entrypoints live in [/Users/tvorogme/projects/kubevpn/cmd/kubevpn/cmds/proxy_out.go](/Users/tvorogme/projects/kubevpn/cmd/kubevpn/cmds/proxy_out.go) and [/Users/tvorogme/projects/kubevpn/cmd/kubevpn/cmds/connect.go](/Users/tvorogme/projects/kubevpn/cmd/kubevpn/cmds/connect.go)
- Managed proxy state handling lives in [/Users/tvorogme/projects/kubevpn/cmd/kubevpn/cmds/proxy_out_manager.go](/Users/tvorogme/projects/kubevpn/cmd/kubevpn/cmds/proxy_out_manager.go)
- Local proxy implementation lives in [/Users/tvorogme/projects/kubevpn/pkg/localproxy](/Users/tvorogme/projects/kubevpn/pkg/localproxy)

## Validation

Recommended validation scenarios:

1. Start `kubevpn proxy-out --listen-socks 127.0.0.1:1080`
2. Access a Service hostname through `socks5h`
3. Access a Service ClusterIP through `socks5h`
4. Start `kubevpn connect --socks`
5. Confirm `kubevpn disconnect` or `kubevpn quit` removes the managed proxy state and kubeconfig file

### Milestone 1

- design doc
- CLI scaffolding
- daemon action scaffolding
- in-memory SOCKS5 server using current connection state

### Milestone 2

- real resolver/dialer integration
- status output
- documentation and examples

### Milestone 3

- HTTP CONNECT
- optional PAC file generation
- optional DNS helper listener

## Why This Is The Right Feature

Today kubevpn is strongest when it can own system routing. Nested VPN environments break that assumption.

A local explicit proxy:

- avoids fighting other VPN route owners
- gives users an app-level integration surface
- reuses kubevpn's existing transport and remote DNS
- solves the exact class of failures seen with `sing-box` packet tunnel on macOS

## Next Implementation Slice

The smallest useful next coding step is:

1. add `kubevpn proxy-out` command and flags
2. add daemon action to attach proxy mode to an existing connection
3. implement TCP-only SOCKS5 CONNECT
4. implement hostname resolution through kubevpn remote DNS
5. verify `curl --proxy socks5h://127.0.0.1:1080` against a cluster service
