# KubeVPN

[English](README.md) | [中文](README_ZH.md) | [维基](https://github.com/wencaiwulue/kubevpn/wiki/%E6%9E%B6%E6%9E%84)

一个本地连接云端 kubernetes 网络的工具，可以在本地直接访问远端集群的服务。也可以在远端集群访问到本地服务，便于调试及开发

## 快速开始

```shell
git clone https://github.com/wencaiwulue/kubevpn.git
cd kubevpn
make kubevpn-linux
make kubevpn-macos
make kubevpn-windows
```

如果你在使用 Windows 系统，可以使用下面这条命令构建：

```shell
go build github.com/wencaiwulue/kubevpn/cmd/kubevpn -o kubevpn.exe
```

如果安装了 Go 1.16 及以上版本，可以使用如下命令安装：

```shell
go install github.com/wencaiwulue/kubevpn/cmd/kubevpn@latest
```

### 安装 bookinfo 作为 demo 应用

```shell
kubectl apply -f https://raw.githubusercontent.com/wencaiwulue/kubevpn/master/samples/bookinfo.yaml
```

## 功能

### 链接到集群网络

```shell
➜  ~ kubevpn connect
INFO[0000] [sudo kubevpn connect]
Password:
2022/02/05 12:09:22 connect.go:303: kubeconfig path: /Users/naison/.kube/config, namespace: default, services: []
2022/02/05 12:09:28 remote.go:47: traffic manager not exist, try to create it...
2022/02/05 12:09:28 remote.go:121: pod kubevpn.traffic.manager status is Pending
2022/02/05 12:09:29 remote.go:121: pod kubevpn.traffic.manager status is Running
Forwarding from 0.0.0.0:10800 -> 10800
2022/02/05 12:09:31 connect.go:171: port forward ready
2022/02/05 12:09:31 connect.go:193: your ip is 223.254.254.176
2022/02/05 12:09:31 connect.go:197: tunnel connected
Handling connection for 10800
2022/02/05 12:09:31 connect.go:211: dns service ok
```

```shell
➜  ~ kubectl get pods -o wide
NAME                          READY   STATUS      RESTARTS   AGE     IP             NODE          NOMINATED NODE   READINESS GATES
details-7db5668668-mq9qr      1/1     Running     0          7m      172.27.0.199   172.30.0.14   <none>           <none>
kubevpn.traffic.manager       1/1     Running     0          74s     172.27.0.207   172.30.0.14   <none>           <none>
productpage-8f9d86644-z8snh   1/1     Running     0          6m59s   172.27.0.206   172.30.0.14   <none>           <none>
ratings-859b96848d-68d7n      1/1     Running     0          6m59s   172.27.0.201   172.30.0.14   <none>           <none>
reviews-dcf754f9d-46l4j       1/1     Running     0          6m59s   172.27.0.202   172.30.0.14   <none>           <none>
```

```shell
➜  ~ ping 172.27.0.206
PING 172.27.0.206 (172.27.0.206): 56 data bytes
64 bytes from 172.27.0.206: icmp_seq=0 ttl=63 time=49.563 ms
64 bytes from 172.27.0.206: icmp_seq=1 ttl=63 time=43.014 ms
64 bytes from 172.27.0.206: icmp_seq=2 ttl=63 time=43.841 ms
64 bytes from 172.27.0.206: icmp_seq=3 ttl=63 time=44.004 ms
64 bytes from 172.27.0.206: icmp_seq=4 ttl=63 time=43.484 ms
^C
--- 172.27.0.206 ping statistics ---
5 packets transmitted, 5 packets received, 0.0% packet loss
round-trip min/avg/max/stddev = 43.014/44.781/49.563/2.415 ms
```

```shell
➜  ~ kubectl get services -o wide
NAME          TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)    AGE     SELECTOR
details       ClusterIP   172.27.255.92    <none>        9080/TCP   9m7s    app=details
productpage   ClusterIP   172.27.255.48    <none>        9080/TCP   9m6s    app=productpage
ratings       ClusterIP   172.27.255.154   <none>        9080/TCP   9m7s    app=ratings
reviews       ClusterIP   172.27.255.155   <none>        9080/TCP   9m6s    app=reviews
```

```shell
➜  ~ curl 172.27.255.48:9080
<!DOCTYPE html>
<html>
  <head>
    <title>Simple Bookstore App</title>
<meta charset="utf-8">
<meta http-equiv="X-UA-Compatible" content="IE=edge">
<meta name="viewport" content="width=device-width, initial-scale=1">
```

### 域名解析功能

```shell
➜  ~ curl productpage.default.svc.cluster.local:9080
<!DOCTYPE html>
<html>
  <head>
    <title>Simple Bookstore App</title>
<meta charset="utf-8">
<meta http-equiv="X-UA-Compatible" content="IE=edge">
<meta name="viewport" content="width=device-width, initial-scale=1">
```

### 短域名解析功能

```shell
➜  ~ curl productpage:9080
<!DOCTYPE html>
<html>
  <head>
    <title>Simple Bookstore App</title>
<meta charset="utf-8">
<meta http-equiv="X-UA-Compatible" content="IE=edge">
...
```

### 反向代理

```shell
➜  ~ kubevpn connect --workloads=service/productpage
INFO[0000] [sudo kubevpn connect --workloads=service/productpage]
Password:
2022/02/05 12:18:22 connect.go:303: kubeconfig path: /Users/naison/.kube/config, namespace: default, services: [service/productpage]
2022/02/05 12:18:28 remote.go:47: traffic manager not exist, try to create it...
2022/02/05 12:18:28 remote.go:121: pod kubevpn.traffic.manager status is Pending
2022/02/05 12:18:29 remote.go:121: pod kubevpn.traffic.manager status is Running
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
deployment "productpage" successfully rolled out
Forwarding from 0.0.0.0:10800 -> 10800
2022/02/05 12:18:34 connect.go:171: port forward ready
2022/02/05 12:18:34 connect.go:193: your ip is 223.254.254.176
2022/02/05 12:18:34 connect.go:197: tunnel connected
Handling connection for 10800
2022/02/05 12:18:35 connect.go:211: dns service ok
```

```go
package main

import (
	"io"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "Hello world!")
	})
	_ = http.ListenAndServe(":9080", nil)
}
```

```shell
➜  ~ curl productpage:9080
Hello world!%
➜  ~ curl productpage.default.svc.cluster.local:9080
Hello world!%
```

### 反向代理支持 service mesh

只支持 HTTP 和 GRPC, 携带了指定 header `"a: 1"` 的流量，将会路由到本地

```shell
➜  ~ kubevpn connect --workloads=service/productpage --mode=mesh --headers a=1
INFO[0000] [sudo kubevpn connect --workloads=service/productpage --mode=mesh --headers a=1]
2022/02/05 12:22:28 connect.go:303: kubeconfig path: /Users/naison/.kube/config, namespace: default, services: [service/productpage]
2022/02/05 12:22:34 remote.go:47: traffic manager not exist, try to create it...
2022/02/05 12:22:34 remote.go:121: pod kubevpn.traffic.manager status is Pending
2022/02/05 12:22:36 remote.go:121: pod kubevpn.traffic.manager status is Running
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
deployment "productpage" successfully rolled out
Forwarding from 0.0.0.0:10800 -> 10800
2022/02/05 12:22:43 connect.go:171: port forward ready
2022/02/05 12:22:43 connect.go:193: your ip is 223.254.254.176
2022/02/05 12:22:43 connect.go:197: tunnel connected
Handling connection for 10800
2022/02/05 12:22:43 connect.go:211: dns service ok
```

```shell
➜  ~ curl productpage:9080
<!DOCTYPE html>
<html>
  <head>
    <title>Simple Bookstore App</title>
<meta charset="utf-8">
<meta http-equiv="X-UA-Compatible" content="IE=edge">
<meta name="viewport" content="width=device-width, initial-scale=1">
...
```

```shell
➜  ~ curl productpage:9080 -H "a: 1"
Hello world!%
```

### 支持多种协议

- TCP
- UDP
- HTTP
- ICMP
- ...

### 支持三大平台

- macOS
- Linux
- Windows

Windows 下需要安装 ```PowerShell```