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
[![Snapcraft](https://snapcraft.io/kubevpn/badge.svg)](https://snapcraft.io/kubevpn)

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

![架构](docs/en/images/kubevpn-proxy-tun-arch.svg)

## 内容

1. [快速开始](./README_ZH.md#快速开始)
2. [功能](./README_ZH.md#功能)
3. [架构](./README_ZH.md#架构)
4. [贡献代码](./README_ZH.md#贡献代码)

## 快速开始

### 使用脚本安装 ( macOS / Linux)

```shell
curl -fsSL https://kubevpn.dev/install.sh | sh
```

### 使用 [brew](https://brew.sh/) 安装 (macOS / Linux)

```shell
brew install kubevpn
```

### 使用 [snap](https://snapcraft.io/kubevpn) 安装 (Linux)

```shell
sudo snap install kubevpn
```

### 使用 [scoop](https://scoop.sh/) (Windows)

```shell
scoop bucket add extras
scoop install kubevpn
```

### 使用 [krew](https://krew.sigs.k8s.io/) (Windows / macOS / Linux)

```shell
kubectl krew index add kubevpn https://github.com/kubenetworks/kubevpn.git
kubectl krew install kubevpn/kubevpn
kubectl kubevpn 
```

### 从 Github release 下载 (Windows / macOS / Linux)

[https://github.com/kubenetworks/kubevpn/releases/latest](https://github.com/kubenetworks/kubevpn/releases/latest)

### 安装 bookinfo 作为 demo 应用

```shell
kubectl apply -f https://raw.githubusercontent.com/kubenetworks/kubevpn/master/samples/bookinfo.yaml
```

## 功能

### 链接到集群网络

使用命令 `kubevpn connect` 链接到集群，请注意这里需要输入电脑密码。因为需要 `root` 权限。(创建虚拟网卡)

```shell
➜  ~ kubevpn connect
Password:
Starting connect
Getting network CIDR from cluster info...
Getting network CIDR from CNI...
Getting network CIDR from services...
Labeling Namespace default
Creating ServiceAccount kubevpn-traffic-manager
Creating Roles kubevpn-traffic-manager
Creating RoleBinding kubevpn-traffic-manager
Creating Service kubevpn-traffic-manager
Creating MutatingWebhookConfiguration kubevpn-traffic-manager
Creating Deployment kubevpn-traffic-manager

Pod kubevpn-traffic-manager-66d969fd45-9zlbp is Pending
Container     Reason            Message
control-plane ContainerCreating
vpn           ContainerCreating
webhook       ContainerCreating

Pod kubevpn-traffic-manager-66d969fd45-9zlbp is Running
Container     Reason           Message
control-plane ContainerRunning
vpn           ContainerRunning
webhook       ContainerRunning

Forwarding port...
Connected tunnel
Adding route...
Configured DNS service
+----------------------------------------------------------+
| Now you can access resources in the kubernetes cluster ! |
+----------------------------------------------------------+
➜  ~
```

提示已经链接到集群了。使用命令 `kubevpn status` 检查一下状态。

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

找一个 pod 的 IP，比如 `productpage-788df7ff7f-jpkcs` 的 IP `172.29.2.134`

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

测试应该可以直接 Ping 通，说明本地可以正常访问到集群网络了。

```shell
➜  ~ kubectl get services -o wide
NAME                      TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)                              AGE     SELECTOR
authors                   ClusterIP   172.21.5.160    <none>        9080/TCP                             114d    app=authors
details                   ClusterIP   172.21.6.183    <none>        9080/TCP                             114d    app=details
kubernetes                ClusterIP   172.21.0.1      <none>        443/TCP                              319d    <none>
kubevpn-traffic-manager   ClusterIP   172.21.2.86     <none>        10800/TCP,9002/TCP,80/TCP            2m28s   app=kubevpn-traffic-manager
productpage               ClusterIP   172.21.10.49    <none>        9080/TCP                             114d    app=productpage
ratings                   ClusterIP   172.21.3.247    <none>        9080/TCP                             114d    app=ratings
reviews                   ClusterIP   172.21.8.24     <none>        9080/TCP                             114d    app=reviews
```

找一个 service 的 IP，比如 `productpage` 的 IP `172.21.10.49`，试着访问一下服务 `productpage`

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

可以看到也可以正常访问，也就是可以在本地访问到集群的 pod 和 service 了～

### 域名解析功能

支持 k8s dns 解析。比如一个名为 `productpage` 的 Pod 或者 Service 处于 `default` 命名空间下可以被如下域名正常解析到：

- `productpage`
- `productpage.default`
- `productpage.default.svc.cluster.local`

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

可以看到能够被正常解析，并且返回相应内容。

### 短域名解析功能

连接到此命名空间下，可以直接使用 `service` name 的方式访问，否则访问其它命令空间下的服务，需要带上命令空间作为域名的一部分，使用如下的域名即可。

- `productpage.default`
- `productpage.default.svc.cluster.local`

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

可以看到直接使用 service name 的方式，可以正常访问到集群资源。

### 链接到多集群网络

有个两个模式

- 模式 `lite`: 可以链接到多个集群网络，但是仅支持链接到多集群。
- 模式 `full`: 不仅支持链接到单个集群网络，还可以拦截工作负载流量到本地电脑。

可以看到已经链接到了一个集群 `ccijorbccotmqodvr189g`，是 `full` 模式

```shell
➜  ~ kubevpn status
ID Mode Cluster               Kubeconfig                 Namespace Status
0  full ccijorbccotmqodvr189g /Users/naison/.kube/config default   Connected
```

```shell
➜  ~ kubevpn connect -n default --kubeconfig ~/.kube/dev_config
Starting connect
Got network CIDR from cache
Use exist traffic manager
Forwarding port...
Connected tunnel
Adding route...
Configured DNS service
+----------------------------------------------------------+
| Now you can access resources in the kubernetes cluster ! |
+----------------------------------------------------------+
```

使用命令 `kubevpn status` 查看当前链接状态。

```shell
➜  ~ kubevpn status
ID Mode Cluster               Kubeconfig                     Namespace Status
0  full ccijorbccotmqodvr189g /Users/naison/.kube/config     default   Connected
1  lite ccidd77aam2dtnc3qnddg /Users/naison/.kube/dev_config default   Connected
➜  ~
```

可以看到连接到了多个集群。

### 反向代理

使用命令 `kubevpn proxy` 代理所有的入站流量到本地电脑。

```shell
➜  ~ kubevpn proxy deployment/productpage
Connected to cluster
Injecting inbound sidecar for deployment/productpage
Checking rollout status for deployment/productpage
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Rollout successfully for deployment/productpage
+----------------------------------------------------------+
| Now you can access resources in the kubernetes cluster ! |
+----------------------------------------------------------+
➜  ~
```

此时在本地使用 `go` 启动一个服务，用于承接流量。

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

使用 `service` name 的方式，直接访问集群中的 `productpage` 服务。

```shell
➜  ~ curl productpage:9080
Hello world!%
➜  ~ curl productpage.default.svc.cluster.local:9080
Hello world!%
```

可以看到直接击中了本地电脑的服务。

### 反向代理支持 service mesh

支持 HTTP, GRPC 和 WebSocket 等, 携带了指定 header `"foo: bar"` 的流量，将会路由到本地

```shell
➜  ~ kubevpn proxy deployment/productpage --headers foo=bar
Connected to cluster
Injecting inbound sidecar for deployment/productpage
Checking rollout status for deployment/productpage
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Rollout successfully for deployment/productpage
+----------------------------------------------------------+
| Now you can access resources in the kubernetes cluster ! |
+----------------------------------------------------------+
➜  ~
```

不带 header 直接访问集群资源，可以看到返回的是集群中的服务内容。

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

带上特定 header 访问集群资源，可以看到返回了本地服务的内容。

```shell
➜  ~ curl productpage:9080 -H "foo: bar"
Hello world!%
```

如果你需要取消代理流量，可以执行如下命令：

```shell
➜  ~ kubevpn leave deployments/productpage
Leaving workload deployments/productpage
Checking rollout status for deployments/productpage
Waiting for deployment "productpage" rollout to finish: 0 out of 1 new replicas have been updated...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Rollout successfully for deployments/productpage
```

### 本地进入开发模式 🐳

将 Kubernetes pod 运行在本地的 Docker 容器中，同时配合 service mesh, 拦截带有指定 header 的流量到本地，或者所有的流量到本地。这个开发模式依赖于本地
Docker。

```shell
➜  ~ kubevpn dev deployment/authors --headers foo=bar --entrypoint sh
Starting connect
Got network CIDR from cache
Use exist traffic manager
Forwarding port...
Connected tunnel
Adding route...
Configured DNS service
Injecting inbound sidecar for deployment/authors
Patching workload deployment/authors
Checking rollout status for deployment/authors
Waiting for deployment "authors" rollout to finish: 0 out of 1 new replicas have been updated...
Waiting for deployment "authors" rollout to finish: 1 old replicas are pending termination...
deployment "authors" successfully rolled out
Rollout successfully for Deployment.apps/authors 
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
{"status":"Authors is healthy"} /opt/microservices # echo "continue testing pod access..."
continue testing pod access...
/opt/microservices # exit
Created container: default_authors
Wait container default_authors to be running...
Container default_authors is running now
Disconnecting from the cluster...
Leaving workload deployments.apps/authors
Disconnecting from the cluster...
Performing cleanup operations
Clearing DNS settings
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
kubevpn dev deployment/authors --no-proxy
```

例如：

```shell
➜  ~ kubevpn dev deployment/authors --no-proxy
Starting connect
Got network CIDR from cache
Use exist traffic manager
Forwarding port...
Connected tunnel
Adding route...
Configured DNS service
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

如果你想指定在本地启动容器的镜像, 可以使用参数 `--dev-image`, 当本地不存在该镜像时,
会从对应的镜像仓库拉取。如果你想指定启动参数，可以使用 `--entrypoint`
参数，替换为你想要执行的命令，比如 `--entrypoint /bin/bash`, 更多使用参数，请参见 `kubevpn dev --help`.

### DinD ( Docker in Docker ) 在 Docker 中使用 kubevpn

如果你想在本地使用 Docker in Docker (DinD) 的方式启动开发模式, 由于程序会读写 `/tmp`
目录，您需要手动添加参数 `-v /tmp:/tmp`, 还有一点需要注意, 如果使用 DinD
模式，为了共享容器网络和 pid, 还需要指定参数 `--network`

例如:

```shell
docker run -it --privileged --sysctl net.ipv6.conf.all.disable_ipv6=0 -v /var/run/docker.sock:/var/run/docker.sock -v /tmp:/tmp -v ~/.kube/config:/root/.kube/config --platform linux/amd64 ghcr.io/kubenetworks/kubevpn:latest
```

```shell
➜  ~ docker run -it --privileged --sysctl net.ipv6.conf.all.disable_ipv6=0 -v /var/run/docker.sock:/var/run/docker.sock -v /tmp:/tmp -v ~/.kube/vke:/root/.kube/config --platform linux/amd64 ghcr.io/kubenetworks/kubevpn:latest
Unable to find image 'ghcr.io/kubenetworks/kubevpn:latest' locally
latest: Pulling from ghcr.io/kubenetworks/kubevpn
9c704ecd0c69: Already exists
4987d0a976b5: Pull complete
8aa94c4fc048: Pull complete
526fee014382: Pull complete
6c1c2bedceb6: Pull complete
97ac845120c5: Pull complete
ca82aef6a9eb: Pull complete
1fd9534c7596: Pull complete
588bd802eb9c: Pull complete
Digest: sha256:368db2e0d98f6866dcefd60512960ce1310e85c24a398fea2a347905ced9507d
Status: Downloaded newer image for ghcr.io/kubenetworks/kubevpn:latest
WARNING: image with reference ghcr.io/kubenetworks/kubevpn was found but does not match the specified platform: wanted linux/amd64, actual: linux/arm64
root@5732124e6447:/app# kubevpn dev deployment/authors --headers user=naison --entrypoint sh
hostname is 5732124e6447
Starting connect
Got network CIDR from cache
Use exist traffic manager
Forwarding port...
Connected tunnel
Adding route...
Configured DNS service
Injecting inbound sidecar for deployment/authors
Patching workload deployment/authors
Checking rollout status for deployment/authors
Waiting for deployment "authors" rollout to finish: 1 old replicas are pending termination...
deployment "authors" successfully rolled out
Rollout successfully for Deployment.apps/authors
tar: removing leading '/' from member names
/tmp/6460902982794789917:/var/run/secrets/kubernetes.io/serviceaccount
tar: Removing leading `/' from member names
tar: Removing leading `/' from hard link targets
/tmp/5028895788722532426:/var/run/secrets/kubernetes.io/serviceaccount
Network mode is container:d0b3dab8912a
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

/opt/microservices # cat > hello.go <<EOF
package main

import (
    "fmt"
    "io"
    "net/http"
)

func main() {
    http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
        _, _ = io.WriteString(writer, "Hello world!")
        fmt.Println(">> Container Received request: %s %s from %s\n", request.Method, request.RequestURI, request.RemoteAddr)
    })
    fmt.Println("Start listening http port 9080 ...")
    _ = http.ListenAndServe(":9080", nil)
}
EOF
/opt/microservices # go build hello.go
/opt/microservices # 
//opt/microservices # ls -alh
total 12M    
drwxr-xr-x    1 root     root          26 Nov  4 10:29 .
drwxr-xr-x    1 root     root          26 Oct 18  2021 ..
-rwxr-xr-x    1 root     root        6.3M Oct 18  2021 app
-rwxr-xr-x    1 root     root        5.8M Nov  4 10:29 hello
-rw-r--r--    1 root     root         387 Nov  4 10:28 hello.go
/opt/microservices # 
/opt/microservices # apk add curl
OK: 8 MiB in 19 packages
/opt/microservices # ./hello &
/opt/microservices # Start listening http port 9080 ...
[2]+  Done                       ./hello
/opt/microservices # curl localhost:9080
>> Container Received request: GET / from 127.0.0.1:41230
Hello world!/opt/microservices # 

/opt/microservices # curl authors:9080/health -H "foo: bar"
>>Received request: GET /health from 198.19.0.109:57930
                                                        Hello world!/opt/microservices # 
/opt/microservices # curl localhost:9080/health
{"status":"Authors is healthy"}/opt/microservices # exit
Created container: default_authors
Wait container default_authors to be running...
Container default_authors is running now
Disconnecting from the cluster...
Leaving workload deployments.apps/authors
Disconnecting from the cluster...
Performing cleanup operations
Clearing DNS settings
root@d0b3dab8912a:/app# exit
exit
➜  ~
```

可以看到实际上是在本地使用 `Docker` 启动了三个容器。

```text
➜  ~ docker ps
CONTAINER ID   IMAGE                                  COMMAND                  CREATED         STATUS         PORTS     NAMES
1cd576b51b66   naison/authors:latest                  "sh"                     4 minutes ago   Up 4 minutes             authors_default_kubevpn_6df5f
56a6793df82d   nginx:latest                           "/docker-entrypoint.…"   4 minutes ago   Up 4 minutes             nginx_default_kubevpn_6df63
d0b3dab8912a   ghcr.io/kubenetworks/kubevpn:v2.0.0    "/bin/bash"              5 minutes ago   Up 5 minutes             upbeat_noyce
➜  ~
```

### 支持多种协议

支持 OSI 模型三层及三层以上的协议，例如：

- TCP
- UDP
- ICMP
- gRPC
- Thrift
- WebSocket
- HTTP
- ...

### 支持三大平台

- macOS
- Linux
- Windows

## 架构

[架构](https://kubevpn.dev/docs/architecture/connect)

## 贡献代码

所有都是欢迎的，只是打开一个问题也是受欢迎的～

如果你想在本地电脑上调试项目，可以按照这样的步骤：

- 使用喜欢的 IDE Debug 启动 daemon 和 sudo daemon 两个后台进程。（本质上是两个 GRPC server）
- 添加断点给文件 `pkg/daemon/action/connect.go:21`
- 新开个终端，执行命令 `make kubevpn`
- 然后运行命令 `./bin/kubevpn connect` 这样将会击中断点

### 支持者

[![JetBrains logo.](https://resources.jetbrains.com/storage/products/company/brand/logos/jetbrains.svg)](https://jb.gg/OpenSourceSupport)

### [捐赠支持](https://kubevpn.dev/zh/docs/donate/)
