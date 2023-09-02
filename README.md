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

[中文](README_ZH.md) | [English](README.md) | [Wiki](https://github.com/KubeNetworks/kubevpn/wiki/Architecture)

KubeVPN is Cloud Native Dev Environment, connect to kubernetes cluster network, you can access remote kubernetes
cluster network, remote
kubernetes cluster service can also access your local service. and more, you can run your kubernetes pod on local Docker
container with same environment、volume、and network. you can develop your application on local PC totally.

## QuickStart

#### Install from GitHub release

[LINK](https://github.com/KubeNetworks/kubevpn/releases/latest)

#### Install from custom krew index

```shell
(
  kubectl krew index add kubevpn https://github.com/KubeNetworks/kubevpn.git && \
  kubectl krew install kubevpn/kubevpn && kubectl kubevpn 
) 
```

#### Install from build it manually

```shell
(
  git clone https://github.com/KubeNetworks/kubevpn.git && \
  cd kubevpn && make kubevpn && ./bin/kubevpn
)

```

### Install bookinfo as demo application

```shell
kubectl apply -f https://raw.githubusercontent.com/KubeNetworks/kubevpn/master/samples/bookinfo.yaml
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

### Reverse proxy with mesh

Support HTTP, GRPC and WebSocket etc. with specific header `"a: 1"` will route to your local machine

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

### Dev mode in local 🐳

Run the Kubernetes pod in the local Docker container, and cooperate with the service mesh to intercept the traffic with
the specified header to the local, or all the traffic to the local.

```shell
➜  ~ kubevpn -n kube-system --headers a=1 -p 9080:9080 -p 80:80 -it --entrypoint sh dev deployment/authors
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

You can see that it will start up two containers with docker, mapping to pod two container, and share port with same
network, you can use `localhost:port`
to access another container. And more, all environment、volume and network are the same as remote kubernetes pod, it is
truly consistent with the kubernetes runtime. Makes develop on local PC comes true.

```shell
➜  ~ docker ps
CONTAINER ID        IMAGE                   COMMAND                  CREATED             STATUS              PORTS                                        NAMES
de9e2f8ab57d        nginx:latest            "/docker-entrypoint.…"   5 seconds ago       Up 5 seconds                                                     nginx_kube-system_kubevpn_e21d8
28aa30e8929e        naison/authors:latest   "./app"                  6 seconds ago       Up 5 seconds        0.0.0.0:80->80/tcp, 0.0.0.0:9080->9080/tcp   authors_kube-system_kubevpn_e21d8
➜  ~
```

If you just want to start up a docker image, you can use simple way like this:

```shell
kubevpn --headers user=naison dev deployment/authors
```

Example：

```shell
root@27b74bde78b6:/app# kubevpn --headers user=naison dev deployment/authors
hostname 27b74bde78b6
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
/tmp/3795398593261835591:/var/run/secrets/kubernetes.io/serviceaccount
tar: Removing leading `/' from member names
tar: Removing leading `/' from hard link targets
/tmp/1432525228828829439:/var/run/secrets/kubernetes.io/serviceaccount
Created container: nginx_default_kubevpn_08aba
Wait container nginx_default_kubevpn_08aba to be running...
Container nginx_default_kubevpn_08aba is running now
WARNING: The requested image's platform (linux/amd64) does not match the detected host platform (linux/arm64/v8) and no specific platform was requested
Created container: authors_default_kubevpn_08ab9
2023/09/02 00:17:00 Start listening http port 9080 ...
```

Now the main process will hang up to show you log.

If you want to specify the image to start the container locally, you can use the parameter `--docker-image`. When the
image does not exist locally, it will be pulled from the corresponding mirror warehouse. If you want to specify startup
parameters, you can use `--entrypoint` parameter, replace it with the command you want to execute, such
as `--entrypoint /bin/bash`, for more parameters, see `kubevpn dev --help`.

### DinD ( Docker in Docker ) use kubevpn in Docker

If you want to start the development mode locally using Docker in Docker (DinD), because the program will read and
write the `/tmp` directory, you need to manually add the parameter `-v /tmp:/tmp` (outer docker) and other thing is you
need to special parameter `--network` (inner docker) for sharing network and pid

Example:

```shell
docker run -it --privileged --sysctl net.ipv6.conf.all.disable_ipv6=0 -v /var/run/docker.sock:/var/run/docker.sock -v /tmp:/tmp -v ~/.kube/config:/root/.kube/config naison/kubevpn:v1.1.36
```

```shell
➜  ~ docker run -it --privileged --sysctl net.ipv6.conf.all.disable_ipv6=0 -c authors -v /var/run/docker.sock:/var/run/docker.sock -v /tmp:/tmp -v ~/.kube/vke:/root/.kube/config naison/kubevpn:v1.1.36
root@4d0c3c4eae2b:/# 
root@4d0c3c4eae2b:/# kubevpn -n kube-system --image naison/kubevpn:v1.1.36 --headers user=naison -it --entrypoint sh dev deployment/authors

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
   60 root      0:07 {kubevpn} /usr/bin/qemu-x86_64 kubevpn kubevpn dev deployment/authors -n kube-system --image naison/kubevpn:v1.1.36 --headers user=naison --parent
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

### Multiple Protocol

- TCP
- UDP
- ICMP
- GRPC
- WebSocket
- HTTP
- ...

### Cross-platform

- macOS
- Linux
- Windows

on Windows platform, you need to
install [PowerShell](https://docs.microsoft.com/en-us/powershell/scripting/install/installing-powershell-on-windows?view=powershell-7.2)
in advance

## FAQ

### 1, What should I do if the dependent image cannot be pulled, or the inner environment cannot access docker.io?

Answer: here are two solution to solve this problem

- Solution 1: In the network that can access docker.io, transfer the image in the command `kubevpn version` to your own
  private image registry, and then add option `--image` to special image when starting the command.
  Example:

``` shell
  ➜  ~ kubevpn version
  KubeVPN: CLI
  Version: v1.1.36
  Image: docker.io/naison/kubevpn:v1.1.36
  Branch: master
  Git commit: 87dac42dad3d8f472a9dcdfc2c6cd801551f23d1
  Built time: 2023-01-15 04:19:45
  Built OS/Arch: linux/amd64
  Built Go version: go1.18.10
  ➜  ~
  ```

Image is `docker.io/naison/kubevpn:v1.1.36`, transfer this image to private docker registry

```text
docker pull docker.io/naison/kubevpn:v1.1.36
docker tag docker.io/naison/kubevpn:v1.1.36 [docker registry]/[namespace]/[repo]:[tag]
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

- Solution 2: Use options `--transfer-image`, enable this flags will transfer image from default image to `--image`
  special address automatically。
  Example

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

### 2, When use `kubevpn dev`, but got error code 137, how to resolve ?

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

This is because of your docker-desktop required resource is less than pod running request resource, it OOM killed, so
you can add more resource in your docker-desktop setting `Preferences --> Resources --> Memory`

### 3, Using WSL( Windows Sub Linux ) Docker, when use mode `kubevpn dev`, can not connect to cluster network, how to solve this problem?

Answer:

this is because WSL'Docker using Windows's Network, so if even start a container in WSL, this container will not use WSL
network, but use Windows network

Solution:

- 1): install docker in WSL, not use Windows Docker-desktop
- 2): use command `kubevpn connect` on Windows, and then startup `kubevpn dev` in WSL
- 3): startup a container using command `kubevpn connect` on Windows, and then
  startup `kubevpn dev --network container:$CONTAINER_ID` in WSL

### 4，After use command `kubevpn dev` enter develop mode，but can't assess kubernetes api-server，occur error `172.17.0.1:443 connect refusued`，how to solve this problem?

Answer:

Maybe k8s network subnet is conflict with docker subnet

Solution:

- Use option `--connect-mode container` to startup command `kubevpn dev`
- Modify `~/.docker/daemon.json`, add not conflict subnet, eg: `"bip": "172.15.0.1/24"`.

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

add subnet not conflict, eg: 172.15.0.1/24

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

restart docker and retry
