# Helm charts for KubeVPN server

Use helm to install kubevpn server means use cluster mode. All user will use this instance.

- Please make sure users should have permission to namespace `kubevpn`.
- Otherwise, will fall back to create `kubevpn` deployment in own namespace.

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
helm install kubevpn kubevpn/kubevpn --set netstack=gvisor -n kubevpn --create-namespace
```