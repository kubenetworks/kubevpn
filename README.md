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

[中文](README_ZH.md) | [English](README.md) | [Wiki](https://github.com/kubenetworks/kubevpn/wiki/Architecture)

KubeVPN offers a Cloud-Native Dev Environment that seamlessly connects to your Kubernetes cluster network.

Gain access to the Kubernetes cluster network effortlessly using service names or Pod IP/Service IP. Facilitate the
interception of inbound traffic from remote Kubernetes cluster services to your local PC through a service mesh and
more.

For instance, you have the flexibility to run your Kubernetes pod within a local Docker container, ensuring an identical
environment, volume, and network setup.
With KubeVPN, empower yourself to develop applications entirely on your local PC!

## Content

1. [QuickStart](./README.md#quickstart)
2. [Functions](./README.md#functions)
3. [FAQ](./README.md#faq)
4. [Architecture](./README.md#architecture)
5. [Contributions](./README.md#Contributions)

## QuickStart

#### Install from brew

```shell
brew install kubevpn
```

#### Install from custom krew index

```shell
(
  kubectl krew index add kubevpn https://github.com/kubenetworks/kubevpn.git && \
  kubectl krew install kubevpn/kubevpn && kubectl kubevpn 
) 
```

#### Install from GitHub release

[LINK](https://github.com/kubenetworks/kubevpn/releases/latest)

#### Install from build it manually

```shell
(
  git clone https://github.com/kubenetworks/kubevpn.git && \
  cd kubevpn && make kubevpn && ./bin/kubevpn
)

```

### Install bookinfo as demo application

```shell
kubectl apply -f https://raw.githubusercontent.com/kubenetworks/kubevpn/master/samples/bookinfo.yaml
```

For clean up after test

```shell
kubectl delete -f https://raw.githubusercontent.com/kubenetworks/kubevpn/master/samples/bookinfo.yaml
```

## Functions

### Connect to k8s cluster network

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
➜  ~ kubevpn status
ID Mode Cluster               Kubeconfig                 Namespace Status
0  full ccijorbccotmqodvr189g /Users/naison/.kube/config default   Connected
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

To access the service in the cluster, service name or you can use the short domain name, such
as `productpage`

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

***Disclaimer:*** This only works on the namespace where kubevpn-traffic-manager is deployed. Otherwise,
use [Domain resolve](./README.md#domain-resolve)

### Connect to multiple kubernetes cluster network

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

### Reverse proxy

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

For local testing, save the following code as `hello.go`

```go
package main

import (
	"fmt"
	"io"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "Hello world!")
		fmt.Printf(">>Received request: %s %s from %s\n", request.Method, request.RequestURI, request.RemoteAddr)
	})
	_ = http.ListenAndServe(":9080", nil)
}
```

and compile it

```
go build hello.go
```

then run it

```
./hello &
```

```shell
export selector=productpage
export pod=`kubectl get pods -l app=${selector} -n default -o jsonpath='{.items[0].metadata.name}'`
export pod_ip=`kubectl get pod $pod -n default -o jsonpath='{.status.podIP}'`
curl -v -H "a: 1" http://$pod_ip:9080/health
```

response would like below

```
❯ curl -v -H "a: 1" http://$pod_ip:9080/health
*   Trying 192.168.72.77:9080...
* Connected to 192.168.72.77 (192.168.72.77) port 9080 (#0)
> GET /health HTTP/1.1
> Host: 192.168.72.77:9080
> User-Agent: curl/7.87.0
> Accept: */*
> a: 1
> 
>>Received request: GET /health from xxx.xxx.xxx.xxx:52974
* Mark bundle as not supporting multiuse
< HTTP/1.1 200 OK
< Date: Sat, 04 Nov 2023 10:19:50 GMT
< Content-Length: 12
< Content-Type: text/plain; charset=utf-8
< 
* Connection #0 to host 192.168.72.77 left intact
Hello world!
```

also you can access via service name

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

first access without header "a: 1", it will access existing pod on kubernetes cluster.

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

Now let's access local service with header `"a: 1"`

```shell
➜  ~ curl productpage:9080 -H "a: 1"
>>Received request: GET / from xxx.xxx.xxx.xxx:51296
Hello world!  
```

If you want to cancel proxy, just run command:

```shell
➜  ~ kubevpn leave deployments/productpage
leave workload deployments/productpage
workload default/deployments/productpage is controlled by a controller
leave workload deployments/productpage successfully
```

### Dev mode in local Docker 🐳

Run the Kubernetes pod in the local Docker container, and cooperate with the service mesh to intercept the traffic with
the specified header to the local, or all the traffic to the local.

```shell
➜  ~ kubevpn dev deployment/authors --headers a=1 --entrypoint sh
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
{"status":"Authors is healthy"} /opt/microservices # echo "continue testing pod access..."
continue testing pod access...
/opt/microservices # exit
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

You can see that it will start up two containers with docker, mapping to pod two container, and share port with same
network, you can use `localhost:port`
to access another container. And more, all environment、volume and network are the same as remote kubernetes pod, it is
truly consistent with the kubernetes runtime. Makes develop on local PC come true.

```shell
➜  ~ docker ps
CONTAINER ID   IMAGE                           COMMAND                  CREATED          STATUS          PORTS                                                                NAMES
afdecf41c08d   naison/authors:latest           "sh"                     37 seconds ago   Up 36 seconds                                                                        authors_default_kubevpn_a9a22
fc04e42799a5   nginx:latest                    "/docker-entrypoint.…"   37 seconds ago   Up 37 seconds   0.0.0.0:80->80/tcp, 0.0.0.0:8888->8888/tcp, 0.0.0.0:9080->9080/tcp   nginx_default_kubevpn_a9a22
➜  ~
```

Here is how to access pod in local docker container

```shell
export authors_pod=`kubectl get pods -l app=authors -n default -o jsonpath='{.items[0].metadata.name}'`
export authors_pod_ip=`kubectl get pod $authors_pod -n default -o jsonpath='{.status.podIP}'`
curl -kv -H "a: 1" http://$authors_pod_ip:80/health
```

Verify logs of nginx container

```shell
docker logs $(docker ps --format '{{.Names}}' | grep nginx_default_kubevpn)
```

If you just want to start up a docker image, you can use a simple way like this:

```shell
kubevpn dev deployment/authors --no-proxy
```

Example：

```shell
➜  ~ kubevpn dev deployment/authors --no-proxy
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

Now the main process will hang up to show you log.

If you want to specify the image to start the container locally, you can use the parameter `--docker-image`. When the
image does not exist locally, it will be pulled from the corresponding mirror warehouse. If you want to specify startup
parameters, you can use `--entrypoint` parameter, replace it with the command you want to execute, such
as `--entrypoint /bin/bash`, for more parameters, see `kubevpn dev --help`.

### DinD ( Docker in Docker ) use kubevpn in Docker

If you want to start the development mode locally using Docker in Docker (DinD), because the program will read and
write the `/tmp` directory, you need to manually add the parameter `-v /tmp:/tmp` (outer docker) and another thing is
you
need to special parameter `--network` (inner docker) for sharing network and pid

Example:

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
root@d0b3dab8912a:/app# kubevpn dev deployment/authors --headers user=naison --entrypoint sh
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

/opt/microservices # curl authors:9080/health -H "a: 1"
>>Received request: GET /health from 223.254.0.109:57930
                                                        Hello world!/opt/microservices # 
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

during test, check what container is running

```text
➜  ~ docker ps
CONTAINER ID   IMAGE                           COMMAND                  CREATED         STATUS         PORTS     NAMES
1cd576b51b66   naison/authors:latest           "sh"                     4 minutes ago   Up 4 minutes             authors_default_kubevpn_6df5f
56a6793df82d   nginx:latest                    "/docker-entrypoint.…"   4 minutes ago   Up 4 minutes             nginx_default_kubevpn_6df63
d0b3dab8912a   naison/kubevpn:v2.0.0     "/bin/bash"              5 minutes ago   Up 5 minutes             upbeat_noyce
➜  ~
```

* For clean up after test

```shell
kubectl delete -f https://raw.githubusercontent.com/kubenetworks/kubevpn/master/samples/bookinfo.yaml
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

## FAQ

### 1, What should I do if the dependent image cannot be pulled, or the inner environment cannot access docker.io?

Answer: here are two solutions to solve this problem

- Solution 1: In the network that can access docker.io, transfer the image in the command `kubevpn version` to your own
  private image registry, and then add option `--image` to special image when starting the command.
  Example:

``` shell
➜  ~ kubevpn version
KubeVPN: CLI
    Version: v2.0.0
    Daemon: v2.0.0
    Image: docker.io/naison/kubevpn:v2.0.0
    Branch: feature/daemon
    Git commit: 7c3a87e14e05c238d8fb23548f95fa1dd6e96936
    Built time: 2023-09-30 22:01:51
    Built OS/Arch: darwin/arm64
    Built Go version: go1.20.5
```

Image is `docker.io/naison/kubevpn:v2.0.0`, transfer this image to private docker registry

```text
docker pull docker.io/naison/kubevpn:v2.0.0
docker tag docker.io/naison/kubevpn:v2.0.0 [docker registry]/[namespace]/[repo]:[tag]
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

### 2, When use `kubevpn dev`, but got error code 137, how to resolve?

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

## Architecture

Architecture can be found [here](/docs/en/Architecture.md).

## Contributions

Always welcome. Just opening an issue should be also grateful.

If you want to debug this project on local PC. Please follow the steps bellow:

- Startup daemon and sudo daemon process with IDE debug mode. (Essentially two GRPC server)
- Add breakpoint to file `pkg/daemon/action/connect.go:21`.
- Open another terminal run `make kubevpn`.
- Then run `./bin/kubevpn connect` and it will hit breakpoint.