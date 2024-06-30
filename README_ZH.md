![kubevpn](https://raw.githubusercontent.com/wencaiwulue/kubevpn/master/samples/flat_log.png)

[![GitHub Workflow][1]](https://github.com/kubenetworks/kubevpn/actions)
[![Go Version][2]](https://github.com/kubenetworks/kubevpn/blob/master/go.mod)
[![Go Report][3]](https://goreportcard.com/report/github.com/wencaiwulue/kubevpn)
[![Maintainability][4]](https://codeclimate.com/github/kubenetworks/kubevpn/maintainability)
[![GitHub License][5]](https://github.com/kubenetworks/kubevpn/blob/main/LICENSE)
[![Docker Pulls][6]](https://hub.docker.com/r/naison/kubevpn)
[![Releases][7]](https://github.com/kubenetworks/kubevpn/releases)
[![GoDoc](https://godoc.org/github.com/kubenetworks/kubevpn?status.png)](https://pkg.go.dev/github.com/wencaiwulue/kubevpn/v2)
[![codecov](https://codecov.io/gh/wencaiwulue/kubevpn/graph/badge.svg?token=KMDSINSDEP)](https://codecov.io/gh/wencaiwulue/kubevpn)

[1]: https://img.shields.io/github/actions/workflow/status/kubenetworks/kubevpn/release.yml?logo=github

[2]: https://img.shields.io/github/go-mod/go-version/kubenetworks/kubevpn?logo=go

[3]: https://goreportcard.com/badge/github.com/wencaiwulue/kubevpn?style=flat

[4]: https://api.codeclimate.com/v1/badges/b5b30239174fc6603aca/maintainability

[5]: https://img.shields.io/github/license/kubenetworks/kubevpn

[6]: https://img.shields.io/docker/pulls/naison/kubevpn?logo=docker

[7]: https://img.shields.io/github/v/release/kubenetworks/kubevpn?logo=smartthings

# KubeVPN

[English](README.md) | [中文](README_ZH.md) | [维基](https://github.com/kubenetworks/kubevpn/wiki/%E6%9E%B6%E6%9E%84)

KubeVPN 提供一个云原生开发环境。通过连接云端 kubernetes 网络，可以在本地使用 k8s dns 或者 Pod IP / Service IP
直接访问远端集群中的服务。拦截远端集群中的工作负载的入流量到本地电脑，配合服务网格便于调试及开发。同时还可以使用开发模式，直接在本地使用
Docker
模拟 k8s pod runtime 将容器运行在本地 (具有相同的环境变量，磁盘和网络)。

## 内容

1. [快速开始](./README_ZH.md#快速开始)
2. [功能](./README_ZH.md#功能)
3. [问答](./README_ZH.md#问答)
4. [架构](./README_ZH.md#架构)
5. [贡献代码](./README_ZH.md#贡献代码)

## 快速开始

## 使用 brew 安装

```shell
brew install kubevpn
```

#### 从 自定义 Krew 仓库安装

```shell
(
  kubectl krew index add kubevpn https://github.com/kubenetworks/kubevpn.git && \
  kubectl krew install kubevpn/kubevpn && kubectl kubevpn 
) 
```

#### 从 Github release 下载编译好的二进制文件

[链接](https://github.com/kubenetworks/kubevpn/releases/latest)

#### 自己构建二进制文件

```shell
(
  git clone https://github.com/kubenetworks/kubevpn.git && \
  cd kubevpn && make kubevpn && ./bin/kubevpn
)

```

#### 安装 bookinfo 作为 demo 应用

```shell
kubectl apply -f https://raw.githubusercontent.com/kubenetworks/kubevpn/master/samples/bookinfo.yaml
```

## 功能

### 链接到集群网络

```shell
➜  ~ kubevpn connect
Password:
start to connect
get cidr from cluster info...
get cidr from cluster info ok
get cidr from cni...
wait pod cni-net-dir-kubevpn to be running timeout, reason , ignore
get cidr from svc...
get cidr from svc ok
get cidr successfully
traffic manager not exist, try to create it...
label namespace default
create serviceAccount kubevpn-traffic-manager
create roles kubevpn-traffic-manager
create roleBinding kubevpn-traffic-manager
create service kubevpn-traffic-manager
create deployment kubevpn-traffic-manager
pod kubevpn-traffic-manager-66d969fd45-9zlbp is Pending
Container     Reason            Message
control-plane ContainerCreating
vpn           ContainerCreating
webhook       ContainerCreating

pod kubevpn-traffic-manager-66d969fd45-9zlbp is Running
Container     Reason           Message
control-plane ContainerRunning
vpn           ContainerRunning
webhook       ContainerRunning

Creating mutatingWebhook_configuration for kubevpn-traffic-manager
update ref count successfully
port forward ready
tunnel connected
dns service ok
+---------------------------------------------------------------------------+
|    Now you can access resources in the kubernetes cluster, enjoy it :)    |
+---------------------------------------------------------------------------+
➜  ~
```

```shell
➜  ~ kubectl get pods -o wide
NAME                                       READY   STATUS             RESTARTS   AGE     IP                NODE              NOMINATED NODE   READINESS GATES
authors-dbb57d856-mbgqk                    3/3     Running            0          7d23h   172.29.2.132      192.168.0.5       <none>           <none>
details-7d8b5f6bcf-hcl4t                   1/1     Running            0          61d     172.29.0.77       192.168.104.255   <none>           <none>
kubevpn-traffic-manager-66d969fd45-9zlbp   3/3     Running            0          74s     172.29.2.136      192.168.0.5       <none>           <none>
productpage-788df7ff7f-jpkcs               1/1     Running            0          61d     172.29.2.134      192.168.0.5       <none>           <none>
ratings-77b6cd4499-zvl6c                   1/1     Running            0          61d     172.29.0.86       192.168.104.255   <none>           <none>
reviews-85c88894d9-vgkxd                   1/1     Running            0          24d     172.29.2.249      192.168.0.5       <none>           <none>
```

```shell
➜  ~ ping 172.29.2.134
PING 172.29.2.134 (172.29.2.134): 56 data bytes
64 bytes from 172.29.2.134: icmp_seq=0 ttl=63 time=55.727 ms
64 bytes from 172.29.2.134: icmp_seq=1 ttl=63 time=56.270 ms
64 bytes from 172.29.2.134: icmp_seq=2 ttl=63 time=55.228 ms
64 bytes from 172.29.2.134: icmp_seq=3 ttl=63 time=54.293 ms
^C
--- 172.29.2.134 ping statistics ---
4 packets transmitted, 4 packets received, 0.0% packet loss
round-trip min/avg/max/stddev = 54.293/55.380/56.270/0.728 ms
```

```shell
➜  ~ kubectl get services -o wide
NAME                      TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)                              AGE     SELECTOR
authors                   ClusterIP   172.21.5.160    <none>        9080/TCP                             114d    app=authors
details                   ClusterIP   172.21.6.183    <none>        9080/TCP                             114d    app=details
kubernetes                ClusterIP   172.21.0.1      <none>        443/TCP                              319d    <none>
kubevpn-traffic-manager   ClusterIP   172.21.2.86     <none>        8422/UDP,10800/TCP,9002/TCP,80/TCP   2m28s   app=kubevpn-traffic-manager
productpage               ClusterIP   172.21.10.49    <none>        9080/TCP                             114d    app=productpage
ratings                   ClusterIP   172.21.3.247    <none>        9080/TCP                             114d    app=ratings
reviews                   ClusterIP   172.21.8.24     <none>        9080/TCP                             114d    app=reviews
```

```shell
➜  ~ curl 172.21.10.49:9080
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

### 链接到多集群网络

```shell
➜  ~ kubevpn status
ID Mode Cluster               Kubeconfig                 Namespace Status
0  full ccijorbccotmqodvr189g /Users/naison/.kube/config default   Connected
```

```shell
➜  ~ kubevpn connect -n default --kubeconfig ~/.kube/dev_config --lite
start to connect
got cidr from cache
get cidr successfully
update ref count successfully
traffic manager already exist, reuse it
port forward ready
tunnel connected
adding route...
dns service ok
+---------------------------------------------------------------------------+
|    Now you can access resources in the kubernetes cluster, enjoy it :)    |
+---------------------------------------------------------------------------+
```

```shell
➜  ~ kubevpn status
ID Mode Cluster               Kubeconfig                     Namespace Status
0  full ccijorbccotmqodvr189g /Users/naison/.kube/config     default   Connected
1  lite ccidd77aam2dtnc3qnddg /Users/naison/.kube/dev_config default   Connected
➜  ~
```

### 反向代理

```shell
➜  ~ kubevpn proxy deployment/productpage
already connect to cluster
start to create remote inbound pod for deployment/productpage
workload default/deployment/productpage is controlled by a controller
rollout status for deployment/productpage
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
deployment "productpage" successfully rolled out
rollout status for deployment/productpage successfully
create remote inbound pod for deployment/productpage successfully
+---------------------------------------------------------------------------+
|    Now you can access resources in the kubernetes cluster, enjoy it :)    |
+---------------------------------------------------------------------------+
➜  ~
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
already connect to cluster
start to create remote inbound pod for deployment/productpage
patch workload default/deployment/productpage with sidecar
rollout status for deployment/productpage
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
deployment "productpage" successfully rolled out
rollout status for deployment/productpage successfully
create remote inbound pod for deployment/productpage successfully
+---------------------------------------------------------------------------+
|    Now you can access resources in the kubernetes cluster, enjoy it :)    |
+---------------------------------------------------------------------------+
➜  ~
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

如果你需要取消代理流量，可以执行如下命令：

```shell
➜  ~ kubevpn leave deployments/productpage
leave workload deployments/productpage
workload default/deployments/productpage is controlled by a controller
leave workload deployments/productpage successfully
```

### 本地进入开发模式 🐳

将 Kubernetes pod 运行在本地的 Docker 容器中，同时配合 service mesh, 拦截带有指定 header 的流量到本地，或者所有的流量到本地。这个开发模式依赖于本地
Docker。

```shell
➜  ~ kubevpn dev deployment/authors --headers a=1 -it --rm --entrypoint sh
connectting to cluster
start to connect
got cidr from cache
get cidr successfully
update ref count successfully
traffic manager already exist, reuse it
port forward ready
tunnel connected
dns service ok
start to create remote inbound pod for Deployment.apps/authors
patch workload default/Deployment.apps/authors with sidecar
rollout status for Deployment.apps/authors
Waiting for deployment "authors" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "authors" rollout to finish: 1 old replicas are pending termination...
deployment "authors" successfully rolled out
rollout status for Deployment.apps/authors successfully
create remote inbound pod for Deployment.apps/authors successfully
tar: removing leading '/' from member names
/var/folders/30/cmv9c_5j3mq_kthx63sb1t5c0000gn/T/4563987760170736212:/var/run/secrets/kubernetes.io/serviceaccount
tar: Removing leading `/' from member names
tar: Removing leading `/' from hard link targets
/var/folders/30/cmv9c_5j3mq_kthx63sb1t5c0000gn/T/4044542168121221027:/var/run/secrets/kubernetes.io/serviceaccount
create docker network 56c25058d4b7498d02c2c2386ccd1b2b127cb02e8a1918d6d24bffd18570200e
Created container: nginx_default_kubevpn_a9a22
Wait container nginx_default_kubevpn_a9a22 to be running...
Container nginx_default_kubevpn_a9a22 is running on port 80/tcp:80 8888/tcp:8888 9080/tcp:9080 now
WARNING: The requested image's platform (linux/amd64) does not match the detected host platform (linux/arm64/v8) and no specific platform was requested
Created main container: authors_default_kubevpn_a9a22
/opt/microservices # ls
app
/opt/microservices # ps -ef
PID   USER     TIME  COMMAND
    1 root      0:00 nginx: master process nginx -g daemon off;
   29 101       0:00 nginx: worker process
   30 101       0:00 nginx: worker process
   31 101       0:00 nginx: worker process
   32 101       0:00 nginx: worker process
   33 101       0:00 nginx: worker process
   34 root      0:00 {sh} /usr/bin/qemu-x86_64 /bin/sh sh
   44 root      0:00 ps -ef
/opt/microservices # apk add curl
fetch https://dl-cdn.alpinelinux.org/alpine/v3.14/main/x86_64/APKINDEX.tar.gz
fetch https://dl-cdn.alpinelinux.org/alpine/v3.14/community/x86_64/APKINDEX.tar.gz
(1/4) Installing brotli-libs (1.0.9-r5)
(2/4) Installing nghttp2-libs (1.43.0-r0)
(3/4) Installing libcurl (8.0.1-r0)
(4/4) Installing curl (8.0.1-r0)
Executing busybox-1.33.1-r3.trigger
OK: 8 MiB in 19 packages
/opt/microservices # ./app &
/opt/microservices # 2023/09/30 13:41:58 Start listening http port 9080 ...

/opt/microservices # curl localhost:9080/health
{"status":"Authors is healthy"}/opt/microservices # exit
prepare to exit, cleaning up
update ref count successfully
tun device closed
leave resource: deployments.apps/authors
workload default/deployments.apps/authors is controlled by a controller
leave resource: deployments.apps/authors successfully
clean up successfully
prepare to exit, cleaning up
update ref count successfully
clean up successfully
➜  ~
```

此时本地会启动两个 container, 对应 pod 容器中的两个 container, 并且共享端口, 可以直接使用 localhost:port 的形式直接访问另一个
container,
并且, 所有的环境变量、挂载卷、网络条件都和 pod 一样, 真正做到与 kubernetes 运行环境一致。

```shell
➜  ~ docker ps
CONTAINER ID   IMAGE                           COMMAND                  CREATED          STATUS          PORTS                                                                NAMES
afdecf41c08d   naison/authors:latest           "sh"                     37 seconds ago   Up 36 seconds                                                                        authors_default_kubevpn_a9a22
fc04e42799a5   nginx:latest                    "/docker-entrypoint.…"   37 seconds ago   Up 37 seconds   0.0.0.0:80->80/tcp, 0.0.0.0:8888->8888/tcp, 0.0.0.0:9080->9080/tcp   nginx_default_kubevpn_a9a22
➜  ~
```

如果你只是想在本地启动镜像，可以用一种简单的方式：

```shell
kubevpn dev deployment/authors --no-proxy -it --rm
```

例如：

```shell
➜  ~ kubevpn dev deployment/authors --no-proxy -it --rm
connectting to cluster
start to connect
got cidr from cache
get cidr successfully
update ref count successfully
traffic manager already exist, reuse it
port forward ready
tunnel connected
dns service ok
tar: removing leading '/' from member names
/var/folders/30/cmv9c_5j3mq_kthx63sb1t5c0000gn/T/5631078868924498209:/var/run/secrets/kubernetes.io/serviceaccount
tar: Removing leading `/' from member names
tar: Removing leading `/' from hard link targets
/var/folders/30/cmv9c_5j3mq_kthx63sb1t5c0000gn/T/1548572512863475037:/var/run/secrets/kubernetes.io/serviceaccount
create docker network 56c25058d4b7498d02c2c2386ccd1b2b127cb02e8a1918d6d24bffd18570200e
Created container: nginx_default_kubevpn_ff34b
Wait container nginx_default_kubevpn_ff34b to be running...
Container nginx_default_kubevpn_ff34b is running on port 80/tcp:80 8888/tcp:8888 9080/tcp:9080 now
WARNING: The requested image's platform (linux/amd64) does not match the detected host platform (linux/arm64/v8) and no specific platform was requested
Created main container: authors_default_kubevpn_ff34b
2023/09/30 14:02:31 Start listening http port 9080 ...

```

此时程序会挂起，默认为显示日志

如果你想指定在本地启动容器的镜像, 可以使用参数 `--docker-image`, 当本地不存在该镜像时,
会从对应的镜像仓库拉取。如果你想指定启动参数，可以使用 `--entrypoint`
参数，替换为你想要执行的命令，比如 `--entrypoint /bin/bash`, 更多使用参数，请参见 `kubevpn dev --help`.

### DinD ( Docker in Docker ) 在 Docker 中使用 kubevpn

如果你想在本地使用 Docker in Docker (DinD) 的方式启动开发模式, 由于程序会读写 `/tmp`
目录，您需要手动添加参数 `-v /tmp:/tmp`, 还有一点需要注意, 如果使用 DinD
模式，为了共享容器网络和 pid, 还需要指定参数 `--network`

例如:

```shell
docker run -it --privileged --sysctl net.ipv6.conf.all.disable_ipv6=0 -v /var/run/docker.sock:/var/run/docker.sock -v /tmp:/tmp -v ~/.kube/config:/root/.kube/config --platform linux/amd64 naison/kubevpn:v2.0.0
```

```shell
➜  ~ docker run -it --privileged --sysctl net.ipv6.conf.all.disable_ipv6=0 -v /var/run/docker.sock:/var/run/docker.sock -v /tmp:/tmp -v ~/.kube/vke:/root/.kube/config --platform linux/amd64 naison/kubevpn:v2.0.0
Unable to find image 'naison/kubevpn:v2.0.0' locally
v2.0.0: Pulling from naison/kubevpn
445a6a12be2b: Already exists
bd6c670dd834: Pull complete
64a7297475a2: Pull complete
33fa2e3224db: Pull complete
e008f553422a: Pull complete
5132e0110ddc: Pull complete
5b2243de1f1a: Pull complete
662a712db21d: Pull complete
4f4fb700ef54: Pull complete
33f0298d1d4f: Pull complete
Digest: sha256:115b975a97edd0b41ce7a0bc1d8428e6b8569c91a72fe31ea0bada63c685742e
Status: Downloaded newer image for naison/kubevpn:v2.0.0
root@d0b3dab8912a:/app# kubevpn dev deployment/authors --headers user=naison -it --entrypoint sh

----------------------------------------------------------------------------------
    Warn: Use sudo to execute command kubevpn can not use user env KUBECONFIG.
    Because of sudo user env and user env are different.
    Current env KUBECONFIG value:
----------------------------------------------------------------------------------

hostname is d0b3dab8912a
connectting to cluster
start to connect
got cidr from cache
get cidr successfully
update ref count successfully
traffic manager already exist, reuse it
port forward ready
tunnel connected
dns service ok
start to create remote inbound pod for Deployment.apps/authors
patch workload default/Deployment.apps/authors with sidecar
rollout status for Deployment.apps/authors
Waiting for deployment "authors" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "authors" rollout to finish: 1 old replicas are pending termination...
deployment "authors" successfully rolled out
rollout status for Deployment.apps/authors successfully
create remote inbound pod for Deployment.apps/authors successfully
tar: removing leading '/' from member names
/tmp/6460902982794789917:/var/run/secrets/kubernetes.io/serviceaccount
tar: Removing leading `/' from member names
tar: Removing leading `/' from hard link targets
/tmp/5028895788722532426:/var/run/secrets/kubernetes.io/serviceaccount
network mode is container:d0b3dab8912a
Created container: nginx_default_kubevpn_6df63
Wait container nginx_default_kubevpn_6df63 to be running...
Container nginx_default_kubevpn_6df63 is running now
WARNING: The requested image's platform (linux/amd64) does not match the detected host platform (linux/arm64/v8) and no specific platform was requested
Created main container: authors_default_kubevpn_6df5f
/opt/microservices # ps -ef
PID   USER     TIME  COMMAND
    1 root      0:00 {bash} /usr/bin/qemu-x86_64 /bin/bash /bin/bash
   14 root      0:02 {kubevpn} /usr/bin/qemu-x86_64 /usr/local/bin/kubevpn kubevpn dev deployment/authors --headers
   25 root      0:01 {kubevpn} /usr/bin/qemu-x86_64 /usr/local/bin/kubevpn /usr/local/bin/kubevpn daemon
   37 root      0:04 {kubevpn} /usr/bin/qemu-x86_64 /usr/local/bin/kubevpn /usr/local/bin/kubevpn daemon --sudo
   53 root      0:00 nginx: master process nginx -g daemon off;
(4/4) Installing curl (8.0.1-r0)
Executing busybox-1.33.1-r3.trigger
OK: 8 MiB in 19 packagesnx: worker process
/opt/microservices #
/opt/microservices # apk add curl
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
/opt/microservices # ls -alh
total 6M
drwxr-xr-x    2 root     root        4.0K Oct 18  2021 .
drwxr-xr-x    1 root     root        4.0K Oct 18  2021 ..
-rwxr-xr-x    1 root     root        6.3M Oct 18  2021 app
/opt/microservices # ./app &
/opt/microservices # 2023/09/30 14:27:32 Start listening http port 9080 ...

/opt/microservices # curl authors:9080/health
/opt/microservices # curl authors:9080/health
{"status":"Authors is healthy"}/opt/microservices #
/opt/microservices # curl localhost:9080/health
{"status":"Authors is healthy"}/opt/microservices # exit
prepare to exit, cleaning up
update ref count successfully
tun device closed
leave resource: deployments.apps/authors
workload default/deployments.apps/authors is controlled by a controller
leave resource: deployments.apps/authors successfully
clean up successfully
prepare to exit, cleaning up
update ref count successfully
clean up successfully
root@d0b3dab8912a:/app# exit
exit
➜  ~
```

```text
➜  ~ docker ps
CONTAINER ID   IMAGE                           COMMAND                  CREATED         STATUS         PORTS     NAMES
1cd576b51b66   naison/authors:latest           "sh"                     4 minutes ago   Up 4 minutes             authors_default_kubevpn_6df5f
56a6793df82d   nginx:latest                    "/docker-entrypoint.…"   4 minutes ago   Up 4 minutes             nginx_default_kubevpn_6df63
d0b3dab8912a   naison/kubevpn:v2.0.0     "/bin/bash"              5 minutes ago   Up 5 minutes             upbeat_noyce
➜  ~
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

## 问答

### 1，依赖的镜像拉不下来，或者内网环境无法访问 docker.io 怎么办？

答：有两种方法可以解决

- 第一种，在可以访问 docker.io 的网络中，将命令 `kubevpn version` 中的 image 镜像，
  转存到自己的私有镜像仓库，然后启动命令的时候，加上 `--image 新镜像` 即可。
  例如:

``` shell
➜  ~ kubevpn version
KubeVPN: CLI
    Version: v2.0.0
    DaemonVersion: v2.0.0
    Image: docker.io/naison/kubevpn:v2.0.0
    Branch: feature/daemon
    Git commit: 7c3a87e14e05c238d8fb23548f95fa1dd6e96936
    Built time: 2023-09-30 22:01:51
    Built OS/Arch: darwin/arm64
    Built Go version: go1.20.5
```

镜像是 `docker.io/naison/kubevpn:v2.0.0`，将此镜像转存到自己的镜像仓库。

```text
docker pull docker.io/naison/kubevpn:v2.0.0
docker tag docker.io/naison/kubevpn:v2.0.0 [镜像仓库地址]/[命名空间]/[镜像仓库]:[镜像版本号]
docker push [镜像仓库地址]/[命名空间]/[镜像仓库]:[镜像版本号]
```

然后就可以使用这个镜像了，如下：

```text
➜  ~ kubevpn connect --image [docker registry]/[namespace]/[repo]:[tag]
got cidr from cache
traffic manager not exist, try to create it...
pod [kubevpn-traffic-manager] status is Running
...
```

- 第二种，使用选项 `--transfer-image`, 这个选项将会自动转存镜像到选项 `--image` 指定的地址。
  例如：

```shell
➜  ~ kubevpn connect --transfer-image --image nocalhost-team-docker.pkg.coding.net/nocalhost/public/kubevpn:v2.0.0
v2.0.0: Pulling from naison/kubevpn
Digest: sha256:450446850891eb71925c54a2fab5edb903d71103b485d6a4a16212d25091b5f4
Status: Image is up to date for naison/kubevpn:v2.0.0
The push refers to repository [nocalhost-team-docker.pkg.coding.net/nocalhost/public/kubevpn]
ecc065754c15: Preparing
f2b6c07cb397: Pushed
448eaa16d666: Pushed
f5507edfc283: Pushed
3b6ea9aa4889: Pushed
ecc065754c15: Pushed
feda785382bb: Pushed
v2.0.0: digest: sha256:85d29ebb53af7d95b9137f8e743d49cbc16eff1cdb9983128ab6e46e0c25892c size: 2000
start to connect
got cidr from cache
get cidr successfully
update ref count successfully
traffic manager already exist, reuse it
port forward ready
tunnel connected
dns service ok
+---------------------------------------------------------------------------+
|    Now you can access resources in the kubernetes cluster, enjoy it :)    |
+---------------------------------------------------------------------------+
➜  ~
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
clean up successfully
```

这是因为你的 `Docker-desktop` 声明的资源, 小于 container 容器启动时所需要的资源, 因此被 OOM 杀掉了,
你可以增加 `Docker-desktop` 对于 resources
的设置, 目录是：`Preferences --> Resources --> Memory`

### 3，使用 WSL( Windows Sub Linux ) Docker, 用命令 `kubevpn dev` 进入开发模式的时候, 在 terminal 中无法提示链接集群网络, 这是为什么, 如何解决?

答案: 这是因为 WSL 的 Docker 使用的是 主机 Windows 的网络, 所以即便在 WSL 中启动 container, 这个 container 不会使用 WSL
的网络，而是使用 Windows 的网络。
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

## 架构

架构信息可以从这里找到 [这里](/docs/en/Architecture.md).

## 贡献代码

所有都是欢迎的，只是打开一个问题也是受欢迎的～

如果你想在本地电脑上调试项目，可以按照这样的步骤：

- 使用喜欢的 IDE Debug 启动 daemon 和 sudo daemon 两个后台进程。（本质上是两个 GRPC server）
- 添加断点给文件 `pkg/daemon/action/connect.go:21`
- 新开个终端，执行命令 `make kubevpn`
- 然后运行命令 `./bin/kubevpn connect` 这样将会击中断点