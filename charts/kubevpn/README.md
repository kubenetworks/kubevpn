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

## AWS Fargate cluster

```shell
helm install kubevpn kubevpn/kubevpn -n kubevpn --create-namespace
```

*Proxy/ServiceMesh mode only support k8s service*

```shell
kubevpn proxy service/authors
```