# KubeVPN

[中文](README_ZH.md) | [English](README.md) | [Wiki](https://github.com/wencaiwulue/kubevpn/wiki/Architecture)

A tools which can connect to kubernetes cluster network, you can access remote kubernetes cluster network, remote
kubernetes cluster service can also access your local service

## QuickStart

#### Install from GitHub release

[link](https://github.com/wencaiwulue/kubevpn/releases/latest)

#### Install from build it manually

```shell
git clone https://github.com/wencaiwulue/kubevpn.git
cd kubevpn
make kubevpn
```

### Install bookinfo as demo application

```shell
kubectl apply -f https://raw.githubusercontent.com/wencaiwulue/kubevpn/master/samples/bookinfo.yaml
```

## Functions

### Connect to k8s cluster network

```shell
➜  ~ kubevpn connect
get cidr from cluster info...
get cidr from cluster info ok
get cidr from cni...
get cidr from svc...
get cidr from svc ok
traffic manager not exist, try to create it...
pod [kubevpn-traffic-manager] status is Pending
Container Reason Message

pod [kubevpn-traffic-manager] status is Pending
Container     Reason            Message
control-plane ContainerCreating
vpn           ContainerCreating
webhook       ContainerCreating

pod [kubevpn-traffic-manager] status is Running
Container     Reason           Message
control-plane ContainerRunning
vpn           ContainerRunning
webhook       ContainerRunning

update ref count successfully
port forward ready
your ip is 223.254.0.101
tunnel connected
dns service ok

---------------------------------------------------------------------------
    Now you can access resources in the kubernetes cluster, enjoy it :)
---------------------------------------------------------------------------

```

**after you see this prompt, then leave this terminal alone, open a new terminal, continue operation**

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

### Domain resolve

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

### Short domain resolve

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

### Reverse proxy

```shell
➜  ~ kubevpn connect --workloads deployment/productpage
got cidr from cache
traffic manager not exist, try to create it...
pod [kubevpn-traffic-manager] status is Running
Container     Reason           Message
control-plane ContainerRunning
vpn           ContainerRunning
webhook       ContainerRunning

update ref count successfully
Waiting for deployment "productpage" rollout to finish: 1 out of 2 new replicas have been updated...
Waiting for deployment "productpage" rollout to finish: 1 out of 2 new replicas have been updated...
Waiting for deployment "productpage" rollout to finish: 1 out of 2 new replicas have been updated...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
deployment "productpage" successfully rolled out
port forward ready
your ip is 223.254.0.101
tunnel connected
dns service ok

---------------------------------------------------------------------------
    Now you can access resources in the kubernetes cluster, enjoy it :)
---------------------------------------------------------------------------

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

### Reverse proxy with mesh

Only support HTTP and GRPC, with specific header `"a: 1"` will route to your local machine

```shell
➜  ~ kubevpn connect --workloads=deployment/productpage --headers a=1
got cidr from cache
traffic manager not exist, try to create it...
pod [kubevpn-traffic-manager] status is Running
Container     Reason           Message
control-plane ContainerRunning
vpn           ContainerRunning
webhook       ContainerRunning

update ref count successfully
Waiting for deployment "productpage" rollout to finish: 1 out of 2 new replicas have been updated...
Waiting for deployment "productpage" rollout to finish: 1 out of 2 new replicas have been updated...
Waiting for deployment "productpage" rollout to finish: 1 out of 2 new replicas have been updated...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
deployment "productpage" successfully rolled out
port forward ready
your ip is 223.254.0.101
tunnel connected
dns service ok

---------------------------------------------------------------------------
    Now you can access resources in the kubernetes cluster, enjoy it :)
---------------------------------------------------------------------------

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

### Multiple Protocol

- TCP
- UDP
- HTTP
- ICMP
- ...

### Cross-platform

- macOS
- Linux
- Windows

on Windows platform, you need to
install [PowerShell](https://docs.microsoft.com/en-us/powershell/scripting/install/installing-powershell-on-windows?view=powershell-7.2)
in advance

## FAQ

- What should I do if the dependent image cannot be pulled, or the inner environment cannot access docker.io?
- Answer: In the network that can access docker.io, transfer the image in the command `kubevpn version` to your own
  private image registry, and then add option `--image` to special image when starting the command.
  Example:

``` shell
  ➜  ~ kubevpn version
  KubeVPN: CLI
  Version: v1.1.14
  Image: docker.io/naison/kubevpn:v1.1.14
  Branch: master
  Git commit: 87dac42dad3d8f472a9dcdfc2c6cd801551f23d1
  Built time: 2023-01-15 04:19:45
  Built OS/Arch: linux/amd64
  Built Go version: go1.18.10
  ➜  ~
  ```

Image is `docker.io/naison/kubevpn:v1.1.14`, transfer this image to private docker registry

```text
docker pull docker.io/naison/kubevpn:v1.1.14
docker tag docker.io/naison/kubevpn:v1.1.14 [docker registry]/[namespace]/[repo]:[tag]
docker push [docker registry]/[namespace]/[repo]:[tag]
```

Then you can use this image, as follows:

```text
➜  ~ kubevpn connect --image [docker registry]/[namespace]/[repo]:[tag]
got cidr from cache
traffic manager not exist, try to create it...
pod [kubevpn-traffic-manager] status is Running
...
```