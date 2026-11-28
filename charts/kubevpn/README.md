# Helm charts for KubeVPN server

Use helm to install kubevpn server means use cluster mode. All user will use this instance.

- Please make sure users should have permission to namespace `kubevpn`.
- Otherwise, will fall back to create `kubevpn` deployment in own namespace.

## Add helm repository kubevpn

```shell
helm repo add kubevpn https://kubenetworks.github.io/charts
```

## Install with default mode

```shell
helm install kubevpn kubevpn/kubevpn -n kubevpn --create-namespace
```

in China, you can use tencent image registry

```shell
helm install kubevpn kubevpn/kubevpn --set image.repository=ccr.ccs.tencentyun.com/kubevpn/kubevpn -n kubevpn --create-namespace
```

## Privileged node-agent requirement

Proxy and Service interception use the `kubevpn-nodeagent` DaemonSet. The manager
namespace must allow privileged pods with `hostPID=true` and a read-only host
`/proc` mount. The agent does not use `hostNetwork`, and workloads/Services are
not modified. Unprivileged AWS Fargate data planes are not supported.