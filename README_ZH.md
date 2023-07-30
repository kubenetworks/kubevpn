![kubevpn](samples/flat_log.png)

[![GitHub Workflow][1]](https://github.com/KubeNetworks/kubevpn/actions)
[![Go Version][2]](https://github.com/KubeNetworks/kubevpn/blob/master/go.mod)
[![Go Report][3]](https://goreportcard.com/badge/github.com/KubeNetworks/kubevpn)
[![Maintainability][4]](https://codeclimate.com/github/KubeNetworks/kubevpn/maintainability)
[![GitHub License][5]](https://github.com/KubeNetworks/kubevpn/blob/main/LICENSE)
[![Docker Pulls][6]](https://hub.docker.com/r/naison/kubevpn)
[![Releases][7]](https://github.com/KubeNetworks/kubevpn/releases)

[1]: https://img.shields.io/github/actions/workflow/status/KubeNetworks/kubevpn/release.yml?logo=github

[2]: https://img.shields.io/github/go-mod/go-version/KubeNetworks/kubevpn?logo=go

[3]: https://img.shields.io/badge/go%20report-A+-brightgreen.svg?style=flat

[4]: https://api.codeclimate.com/v1/badges/b5b30239174fc6603aca/maintainability

[5]: https://img.shields.io/github/license/KubeNetworks/kubevpn

[6]: https://img.shields.io/docker/pulls/naison/kubevpn?logo=docker

[7]: https://img.shields.io/github/v/release/KubeNetworks/kubevpn?logo=smartthings

# KubeVPN

[English](README.md) | [中文](README_ZH.md) | [维基](https://github.com/KubeNetworks/kubevpn/wiki/%E6%9E%B6%E6%9E%84)

KubeVPN 是一个云原生开发工具, 可以在本地连接云端 kubernetes 网络的工具，可以在本地直接访问远端集群的服务。也可以在远端集群访问到本地服务，便于调试及开发。同时还可以使用开发模式，直接在本地使用 Docker
将远程容器运行在本地。

## 快速开始

#### 从 Github release 下载编译好的二进制文件

[链接](https://github.com/KubeNetworks/kubevpn/releases/latest)

#### 从 自定义 Krew 仓库安装

```shell
(
  kubectl krew index add kubevpn https://github.com/KubeNetworks/kubevpn.git && \
  kubectl krew install kubevpn/kubevpn && kubectl kubevpn 
) 
```

#### 自己构建二进制文件

```shell
(
  git clone https://github.com/KubeNetworks/kubevpn.git && \
  cd kubevpn && make kubevpn && ./bin/kubevpn
)

```

#### 安装 bookinfo 作为 demo 应用

```shell
kubectl apply -f https://raw.githubusercontent.com/KubeNetworks/kubevpn/master/samples/bookinfo.yaml
```

## 功能

### 链接到集群网络

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

**有这个提示出来后, 当前 terminal 不要关闭，新打开一个 terminal, 执行新的操作**

```shell
➜  ~ kubectl get pods -o wide
NAME                                     READY   STATUS      RESTARTS   AGE     IP             NODE          NOMINATED NODE   READINESS GATES
details-7db5668668-mq9qr                 1/1     Running     0          7m      172.27.0.199   172.30.0.14   <none>           <none>
kubevpn-traffic-manager-99f8c8d77-x9xjt  1/1     Running     0          74s     172.27.0.207   172.30.0.14   <none>           <none>
productpage-8f9d86644-z8snh              1/1     Running     0          6m59s   172.27.0.206   172.30.0.14   <none>           <none>
ratings-859b96848d-68d7n                 1/1     Running     0          6m59s   172.27.0.201   172.30.0.14   <none>           <none>
reviews-dcf754f9d-46l4j                  1/1     Running     0          6m59s   172.27.0.202   172.30.0.14   <none>           <none>
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
➜  ~ kubevpn proxy deployment/productpage
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

### 反向代理支持 service mesh

支持 HTTP, GRPC 和 WebSocket 等, 携带了指定 header `"a: 1"` 的流量，将会路由到本地

```shell
➜  ~ kubevpn proxy deployment/productpage --headers a=1
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

### 本地进入开发模式 🐳

将 Kubernetes pod 运行在本地的 Docker 容器中，同时配合 service mesh, 拦截带有指定 header 的流量到本地，或者所有的流量到本地。这个开发模式依赖于本地 Docker。

```shell
➜  ~ kubevpn -n kube-system --headers a=1 -p 9080:9080 -p 80:80 dev deployment/authors
got cidr from cache
update ref count successfully
traffic manager already exist, reuse it
Waiting for deployment "authors" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "authors" rollout to finish: 1 old replicas are pending termination...
deployment "authors" successfully rolled out
port forward ready
tunnel connected
dns service ok
tar: removing leading '/' from member names
/var/folders/4_/wt19r8113kq_mfws8sb_w1z00000gn/T/3264799524258261475:/var/run/secrets/kubernetes.io/serviceaccount
tar: Removing leading '/' from member names
tar: Removing leading '/' from hard link targets
/var/folders/4_/wt19r8113kq_mfws8sb_w1z00000gn/T/4472770436329940969:/var/run/secrets/kubernetes.io/serviceaccount
tar: Removing leading '/' from member names
tar: Removing leading '/' from hard link targets
/var/folders/4_/wt19r8113kq_mfws8sb_w1z00000gn/T/359584695576599326:/var/run/secrets/kubernetes.io/serviceaccount
Created container: authors_kube-system_kubevpn_a7d82
Wait container authors_kube-system_kubevpn_a7d82 to be running...
Container authors_kube-system_kubevpn_a7d82 is running on port 9080/tcp:32771 now
Created container: nginx_kube-system_kubevpn_a7d82
Wait container nginx_kube-system_kubevpn_a7d82 to be running...
Container nginx_kube-system_kubevpn_a7d82 is running now
/opt/microservices # ls
app
/opt/microservices # ps -ef
PID   USER     TIME  COMMAND
    1 root      0:00 ./app
   10 root      0:00 nginx: master process nginx -g daemon off;
   32 root      0:00 /bin/sh
   44 101       0:00 nginx: worker process
   45 101       0:00 nginx: worker process
   46 101       0:00 nginx: worker process
   47 101       0:00 nginx: worker process
   49 root      0:00 ps -ef
/opt/microservices # apk add curl
fetch https://dl-cdn.alpinelinux.org/alpine/v3.14/main/x86_64/APKINDEX.tar.gz
fetch https://dl-cdn.alpinelinux.org/alpine/v3.14/community/x86_64/APKINDEX.tar.gz
(1/4) Installing brotli-libs (1.0.9-r5)
(2/4) Installing nghttp2-libs (1.43.0-r0)
(3/4) Installing libcurl (7.79.1-r5)
(4/4) Installing curl (7.79.1-r5)
Executing busybox-1.33.1-r3.trigger
OK: 8 MiB in 19 packages
/opt/microservices # curl localhost:9080
404 page not found
/opt/microservices # curl localhost:9080/health
{"status":"Authors is healthy"}/opt/microservices # exit
prepare to exit, cleaning up
update ref count successfully
clean up successful
```

此时本地会启动两个 container, 对应 pod 容器中的两个 container, 并且共享端口, 可以直接使用 localhost:port 的形式直接访问另一个 container,
并且, 所有的环境变量、挂载卷、网络条件都和 pod 一样, 真正做到与 kubernetes 运行环境一致。

```shell
➜  ~ docker ps
CONTAINER ID        IMAGE                   COMMAND                  CREATED             STATUS              PORTS                                        NAMES
de9e2f8ab57d        nginx:latest            "/docker-entrypoint.…"   5 seconds ago       Up 5 seconds                                                     nginx_kube-system_kubevpn_e21d8
28aa30e8929e        naison/authors:latest   "./app"                  6 seconds ago       Up 5 seconds        0.0.0.0:80->80/tcp, 0.0.0.0:9080->9080/tcp   authors_kube-system_kubevpn_e21d8
➜  ~
```

如果你想指定在本地启动容器的镜像, 可以使用参数 `--docker-image`, 当本地不存在该镜像时, 会从对应的镜像仓库拉取。如果你想指定启动参数，可以使用 `--entrypoint`
参数，替换为你想要执行的命令，比如 `--entrypoint /bin/bash`, 更多使用参数，请参见 `kubevpn dev --help`.

### DinD ( Docker in Docker ) 在 Docker 中使用 kubevpn

如果你想在本地使用 Docker in Docker (DinD) 的方式启动开发模式, 由于程序会读写 `/tmp` 目录，您需要手动添加参数 `-v /tmp:/tmp`, 还有一点需要注意, 如果使用 DinD
模式，为了共享容器网络和 pid, 还需要指定参数 `--network`

例如:

```shell
docker run -it --privileged --sysctl net.ipv6.conf.all.disable_ipv6=0 -v /var/run/docker.sock:/var/run/docker.sock -v /tmp:/tmp -v /Users/naison/.kube/config:/root/.kube/config naison/kubevpn:v1.1.35
```

```shell
➜  ~ docker run -it --privileged --sysctl net.ipv6.conf.all.disable_ipv6=0 -c authors -v /var/run/docker.sock:/var/run/docker.sock -v /tmp:/tmp -v /Users/naison/.kube/config:/root/.kube/config naison/kubevpn:v1.1.35
root@4d0c3c4eae2b:/# hostname
4d0c3c4eae2b
root@4d0c3c4eae2b:/# kubevpn -n kube-system --image naison/kubevpn:v1.1.35 --headers user=naison --network container:4d0c3c4eae2b --entrypoint /bin/bash  dev deployment/authors

----------------------------------------------------------------------------------
    Warn: Use sudo to execute command kubevpn can not use user env KUBECONFIG.
    Because of sudo user env and user env are different.
    Current env KUBECONFIG value:
----------------------------------------------------------------------------------

got cidr from cache
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
Waiting for deployment "authors" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "authors" rollout to finish: 1 old replicas are pending termination...
deployment "authors" successfully rolled out
port forward ready
tunnel connected
dns service ok
tar: removing leading '/' from member names
/tmp/3122262358661539581:/var/run/secrets/kubernetes.io/serviceaccount
tar: Removing leading '/' from member names
tar: Removing leading '/' from hard link targets
/tmp/7677066538742627822:/var/run/secrets/kubernetes.io/serviceaccount
latest: Pulling from naison/authors
Digest: sha256:2e7b2d6a4c6143cde888fcdb70ba091d533e11de70e13e151adff7510a5d52d4
Status: Downloaded newer image for naison/authors:latest
Created container: authors_kube-system_kubevpn_c68e4
Wait container authors_kube-system_kubevpn_c68e4 to be running...
Container authors_kube-system_kubevpn_c68e4 is running now
Created container: nginx_kube-system_kubevpn_c68e7
Wait container nginx_kube-system_kubevpn_c68e7 to be running...
Container nginx_kube-system_kubevpn_c68e7 is running now
/opt/microservices # ps -ef
PID   USER     TIME  COMMAND
    1 root      0:00 {bash} /usr/bin/qemu-x86_64 /bin/bash /bin/bash
   60 root      0:07 {kubevpn} /usr/bin/qemu-x86_64 kubevpn kubevpn dev deployment/authors -n kube-system --image naison/kubevpn:v1.1.35 --headers user=naison --parent
   73 root      0:00 {tail} /usr/bin/qemu-x86_64 /usr/bin/tail tail -f /dev/null
   80 root      0:00 {nginx} /usr/bin/qemu-x86_64 /usr/sbin/nginx nginx -g daemon off;
   92 root      0:00 {sh} /usr/bin/qemu-x86_64 /bin/sh /bin/sh
  156 101       0:00 {nginx} /usr/bin/qemu-x86_64 /usr/sbin/nginx nginx -g daemon off;
  158 101       0:00 {nginx} /usr/bin/qemu-x86_64 /usr/sbin/nginx nginx -g daemon off;
  160 101       0:00 {nginx} /usr/bin/qemu-x86_64 /usr/sbin/nginx nginx -g daemon off;
  162 101       0:00 {nginx} /usr/bin/qemu-x86_64 /usr/sbin/nginx nginx -g daemon off;
  164 root      0:00 ps -ef
/opt/microservices # ls
app
/opt/microservices # apk add curl
fetch https://dl-cdn.alpinelinux.org/alpine/v3.14/main/x86_64/APKINDEX.tar.gz
fetch https://dl-cdn.alpinelinux.org/alpine/v3.14/community/x86_64/APKINDEX.tar.gz
(1/4) Installing brotli-libs (1.0.9-r5)
(2/4) Installing nghttp2-libs (1.43.0-r0)
(3/4) Installing libcurl (7.79.1-r5)
(4/4) Installing curl (7.79.1-r5)
Executing busybox-1.33.1-r3.trigger
OK: 8 MiB in 19 packages
/opt/microservices # curl localhost:80
<!DOCTYPE html>
<html>
<head>
<title>Welcome to nginx!</title>
<style>
html { color-scheme: light dark; }
body { width: 35em; margin: 0 auto;
font-family: Tahoma, Verdana, Arial, sans-serif; }
</style>
</head>
<body>
<h1>Welcome to nginx!</h1>
<p>If you see this page, the nginx web server is successfully installed and
working. Further configuration is required.</p>

<p>For online documentation and support please refer to
<a href="http://nginx.org/">nginx.org</a>.<br/>
Commercial support is available at
<a href="http://nginx.com/">nginx.com</a>.</p>

<p><em>Thank you for using nginx.</em></p>
</body>
</html>
/opt/microservices # ls
app
/opt/microservices # exit
prepare to exit, cleaning up
update ref count successfully
ref-count is zero, prepare to clean up resource
clean up successful
root@4d0c3c4eae2b:/# exit
exit
```

### 支持多种协议

- TCP
- UDP
- ICMP
- GRPC
- WebSocket
- HTTP
- ...

### 支持三大平台

- macOS
- Linux
- Windows

Windows
下需要安装 [PowerShell](https://docs.microsoft.com/zh-cn/powershell/scripting/install/installing-powershell-on-windows?view=powershell-7.2)

## 问答

### 1，依赖的镜像拉不下来，或者内网环境无法访问 docker.io 怎么办？

答：有两种方法可以解决

- 第一种，在可以访问 docker.io 的网络中，将命令 `kubevpn version` 中的 image 镜像， 转存到自己的私有镜像仓库，然后启动命令的时候，加上 `--image 新镜像` 即可。
  例如:

``` shell
  ➜  ~ kubevpn version
  KubeVPN: CLI
  Version: v1.1.35
  Image: docker.io/naison/kubevpn:v1.1.35
  Branch: master
  Git commit: 87dac42dad3d8f472a9dcdfc2c6cd801551f23d1
  Built time: 2023-01-15 04:19:45
  Built OS/Arch: linux/amd64
  Built Go version: go1.18.10
  ➜  ~
  ```

镜像是 `docker.io/naison/kubevpn:v1.1.35`，将此镜像转存到自己的镜像仓库。

```text
docker pull docker.io/naison/kubevpn:v1.1.35
docker tag docker.io/naison/kubevpn:v1.1.35 [镜像仓库地址]/[命名空间]/[镜像仓库]:[镜像版本号]
docker push [镜像仓库地址]/[命名空间]/[镜像仓库]:[镜像版本号]
```

然后就可以使用这个镜像了，如下：

```text
➜  ~ kubevpn connect --image docker.io/naison/kubevpn:v1.1.35
got cidr from cache
traffic manager not exist, try to create it...
pod [kubevpn-traffic-manager] status is Running
...
```

- 第二种，使用选项 `--transfer-image`, 这个选项将会自动转存镜像到选项 `--image` 指定的地址。
  例如：

```shell
➜  ~ kubevpn connect --transfer-image --image nocalhost-team-docker.pkg.coding.net/nocalhost/public/kubevpn:v1.1.33
Password:
v1.1.33: Pulling from naison/kubevpn
Digest: sha256:970c0c82a2d9cbac1595edb56a31e8fc84e02712c00a7211762efee5f66ea70c
Status: Image is up to date for naison/kubevpn:v1.1.33
The push refers to repository [nocalhost-team-docker.pkg.coding.net/nocalhost/public/kubevpn]
9d72fec6b077: Pushed
12a6a77eb79e: Pushed
c7d0f62ec57f: Pushed
5605cea4b7c8: Pushed
4231fec7b258: Pushed
babe72b5fcae: Pushed
6caa74b4bcf0: Pushed
b8a36d10656a: Pushed
v1.1.33: digest: sha256:1bc5e589bec6dc279418009b5e82ce0fd29a2c0e8b9266988964035ad7fbeba5 size: 2000
got cidr from cache
update ref count successfully
traffic manager already exist, reuse it
port forward ready
tunnel connected
dns service ok

+---------------------------------------------------------------------------+
|    Now you can access resources in the kubernetes cluster, enjoy it :)    |
+---------------------------------------------------------------------------+

```

### 2，在使用 `kubevpn dev` 进入开发模式的时候,有出现报错 137, 改怎么解决 ?

```text
dns service ok
tar: Removing leading `/' from member names
tar: Removing leading `/' from hard link targets
/var/folders/30/cmv9c_5j3mq_kthx63sb1t5c0000gn/T/7375606548554947868:/var/run/secrets/kubernetes.io/serviceaccount
Created container: server_vke-system_kubevpn_0db84
Wait container server_vke-system_kubevpn_0db84 to be running...
Container server_vke-system_kubevpn_0db84 is running on port 8888/tcp: 6789/tcp:6789 now
$ Status: , Code: 137
prepare to exit, cleaning up
port-forward occurs error, err: lost connection to pod, retrying
update ref count successfully
ref-count is zero, prepare to clean up resource
clean up successful
```

这是因为你的 `Docker-desktop` 声明的资源, 小于 container 容器启动时所需要的资源, 因此被 OOM 杀掉了, 你可以增加 `Docker-desktop` 对于 resources
的设置, 目录是：`Preferences --> Resources --> Memory`

### 3，使用 WSL( Windows Sub Linux ) Docker, 用命令 `kubevpn dev` 进入开发模式的时候, 在 terminal 中无法提示链接集群网络, 这是为什么, 如何解决?

答案: 这是因为 WSL 的 Docker 使用的是 主机 Windows 的网络, 所以即便在 WSL 中启动 container, 这个 container 不会使用 WSL 的网络，而是使用 Windows 的网络。
解决方案:

- 1): 在 WSL 中安装 Docker, 不要使用 Windows 版本的 Docker-desktop
- 2): 在主机 Windows 使用命令 `kubevpn connect`, 然后在 WSL 中使用 `kubevpn dev` 进入开发模式
- 3): 在主机 Windows 上启动一个 container，在 container 中使用命令 `kubevpn connect`, 然后在 WSL
  中使用 `kubevpn dev --network container:$CONTAINER_ID`

### 4，在使用 `kubevpn dev` 进入开发模式后，无法访问容器网络，出现错误 `172.17.0.1:443 connect refusued`，该如何解决？

答案：大概率是因为 k8s 容器网络和 docker 网络网段冲突了。

解决方案：

- 使用参数 `--connect-mode container` 在容器中链接，也可以解决此问题
- 可以修改文件 `~/.docker/daemon.json` 增加不冲突的网络，例如 `"bip": "172.15.0.1/24"`.

```shell
➜  ~ cat ~/.docker/daemon.json
{
  "builder": {
    "gc": {
      "defaultKeepStorage": "20GB",
      "enabled": true
    }
  },
  "experimental": false,
  "features": {
    "buildkit": true
  },
  "insecure-registries": [
  ],
}
```

增加不冲突的网段

```shell
➜  ~ cat ~/.docker/daemon.json
{
  "builder": {
    "gc": {
      "defaultKeepStorage": "20GB",
      "enabled": true
    }
  },
  "experimental": false,
  "features": {
    "buildkit": true
  },
  "insecure-registries": [
  ],
  "bip": "172.15.0.1/24"
}
```

重启 docker，重新操作即可
