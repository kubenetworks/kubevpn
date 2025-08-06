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

[中文](README_ZH.md) | [English](README.md) | [Wiki](https://github.com/kubenetworks/kubevpn/wiki/Architecture)

KubeVPN offers a Cloud-Native Dev Environment that seamlessly connects to your Kubernetes cluster network.

Gain access to the Kubernetes cluster network effortlessly using service names or Pod IP/Service IP. Facilitate the
interception of inbound traffic from remote Kubernetes cluster services to your local PC through a service mesh and
more.

For instance, you have the flexibility to run your Kubernetes pod within a local Docker container, ensuring an identical
environment, volume, and network setup.
With KubeVPN, empower yourself to develop applications entirely on your local PC!

![arch](docs/en/images/kubevpn-proxy-tun-arch.svg)

## Content

1. [QuickStart](./README.md#quickstart)
2. [Functions](./README.md#functions)
3. [Architecture](./README.md#architecture)
4. [Contributions](./README.md#Contributions)

## QuickStart

### Install from script ( macOS / Linux)

```shell
curl -fsSL https://kubevpn.dev/install.sh | sh
```

### Install from [brew](https://brew.sh/) (macOS / Linux)

```shell
brew install kubevpn
```

### Install from [snap](https://snapcraft.io/kubevpn) (Linux)

```shell
sudo snap install kubevpn
```

### Install from [scoop](https://scoop.sh/) (Windows)

```shell
scoop bucket add extras
scoop install kubevpn
```

### Install from [krew](https://krew.sigs.k8s.io/) (Windows / macOS / Linux)

```shell
kubectl krew index add kubevpn https://github.com/kubenetworks/kubevpn.git
kubectl krew install kubevpn/kubevpn 
kubectl kubevpn 
```

### Install from GitHub release (Windows / macOS / Linux)

[https://github.com/kubenetworks/kubevpn/releases/latest](https://github.com/kubenetworks/kubevpn/releases/latest)

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

use command `kubevpn connect` connect to k8s cluster network, prompt `Password:` need to input computer
password. to enable root operation (create a tun device).

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
Now you can access resources in the kubernetes cluster !
➜  ~
```

already connected to cluster network, use command `kubevpn status` to check status

```shell
➜  ~ kubevpn status
CURRENT   CONNECTION ID   CLUSTER                 KUBECONFIG                      NAMESPACE   STATUS      NETIF
*         03dc50feb8c3    ccijorbccotmqodvr189g   /Users/naison/.kube/config      default     connected   utun4
➜  ~
```

use pod `productpage-788df7ff7f-jpkcs` IP `172.29.2.134`

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

use `ping` to test connection, seems good

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

use service `productpage` IP `172.21.10.49`

```shell
➜  ~ kubectl get services -o wide
NAME                      TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)                              AGE     SELECTOR
authors                   ClusterIP   172.21.5.160    <none>        9080/TCP                             114d    app=authors
details                   ClusterIP   172.21.6.183    <none>        9080/TCP                             114d    app=details
kubernetes                ClusterIP   172.21.0.1      <none>        443/TCP                              319d    <none>
kubevpn-traffic-manager   ClusterIP   172.21.2.86     <none>        10801/TCP,9002/TCP,80/TCP            2m28s   app=kubevpn-traffic-manager
productpage               ClusterIP   172.21.10.49    <none>        9080/TCP                             114d    app=productpage
ratings                   ClusterIP   172.21.3.247    <none>        9080/TCP                             114d    app=ratings
reviews                   ClusterIP   172.21.8.24     <none>        9080/TCP                             114d    app=reviews
```

use command `curl` to test service connection

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

seems good too~

### Domain resolve

support k8s dns name resolve.

a Pod/Service named `productpage` in the `default` namespace can successfully resolve by following name:

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

already connected cluster `ccijorbccotmqodvr189g`

```shell
➜  ~ kubevpn status
CURRENT   CONNECTION ID   CLUSTER                 KUBECONFIG                      NAMESPACE   STATUS      NETIF
*         03dc50feb8c3    ccijorbccotmqodvr189g   /Users/naison/.kube/config      default     connected   utun4
```

then connect to another cluster `ccidd77aam2dtnc3qnddg`

```shell
➜  ~ kubevpn connect -n default --kubeconfig ~/.kube/dev_config
Starting connect
Got network CIDR from cache
Use exist traffic manager
Forwarding port...
Connected tunnel
Adding route...
Configured DNS service
Now you can access resources in the kubernetes cluster !
```

use command `kubevpn status` to check connection status

```shell
➜  ~ kubevpn status
CURRENT   CONNECTION ID   CLUSTER                 KUBECONFIG                      NAMESPACE   STATUS      NETIF
          03dc50feb8c3    ccijorbccotmqodvr189g   /Users/naison/.kube/config      default     connected   utun4
*         86bfdef0ed05    ccidd77aam2dtnc3qnddg   /Users/naison/.kube/dev_config  default     connected   utun5
➜  ~
```

### Reverse proxy

use command `kubevpn proxy` to proxy all inbound traffic to local computer.

```shell
➜  ~ kubevpn proxy deployment/productpage
Connected to cluster
Injecting inbound sidecar for deployment/productpage
Checking rollout status for deployment/productpage
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Rollout successfully for deployment/productpage
Now you can access resources in the kubernetes cluster !
➜  ~
```

show status

```shell
➜  ~ kubevpn status
CURRENT   CONNECTION ID   CLUSTER                 KUBECONFIG                   NAMESPACE   STATUS      NETIF
*         03dc50feb8c3    ccijorbccotmqodvr189g   /Users/naison/.kube/config   default     connected   utun4

          CONNECTION ID   NAMESPACE   NAME                           HEADERS   PORTS        CURRENT PC
          03dc50feb8c3    default     deployments.apps/productpage   *         9080->9080   true
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
curl -v -H "foo: bar" http://$pod_ip:9080/health
```

response would like below

```
❯ curl -v -H "foo: bar" http://$pod_ip:9080/health
*   Trying 192.168.72.77:9080...
* Connected to 192.168.72.77 (192.168.72.77) port 9080 (#0)
> GET /health HTTP/1.1
> Host: 192.168.72.77:9080
> User-Agent: curl/7.87.0
> Accept: */*
> foo: bar
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

Support HTTP, GRPC and WebSocket etc. with specific header `"foo: bar"` will route to your local machine

```shell
➜  ~ kubevpn proxy deployment/productpage --headers foo=bar
Connected to cluster
Injecting inbound sidecar for deployment/productpage
Checking rollout status for deployment/productpage
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Rollout successfully for deployment/productpage
Now you can access resources in the kubernetes cluster !
➜  ~
```

show status

```shell
➜  ~ kubevpn status
CURRENT   CONNECTION ID   CLUSTER                 KUBECONFIG                   NAMESPACE   STATUS      NETIF
*         03dc50feb8c3    ccijorbccotmqodvr189g   /Users/naison/.kube/config   default     connected   utun4

          CONNECTION ID   NAMESPACE   NAME                           HEADERS   PORTS        CURRENT PC
          03dc50feb8c3    default     deployments.apps/productpage   foo=bar   9080->9080   true
➜  ~
```

first access without header "foo: bar", it will access existing pod on kubernetes cluster.

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

Now let's access local service with header `"foo: bar"`

```shell
➜  ~ curl productpage:9080 -H "foo: bar"
>>Received request: GET / from xxx.xxx.xxx.xxx:51296
Hello world!  
```

If you want to cancel proxy, just run command:

```shell
➜  ~ kubevpn leave deployments/productpage
Leaving workload deployments/productpage
Checking rollout status for deployments/productpage
Waiting for deployment "productpage" rollout to finish: 0 out of 1 new replicas have been updated...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Waiting for deployment "productpage" rollout to finish: 1 old replicas are pending termination...
Rollout successfully for deployments/productpage
```

### Run mode in local Docker 🐳

Run the Kubernetes pod in the local Docker container, and cooperate with the service mesh to intercept the traffic with
the specified header to the local, or all the traffic to the local.

```shell
➜  ~ kubevpn run deployment/authors --headers foo=bar --entrypoint sh
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
curl -kv -H "foo: bar" http://$authors_pod_ip:80/health
```

Verify logs of nginx container

```shell
docker logs $(docker ps --format '{{.Names}}' | grep nginx_default_kubevpn)
```

If you just want to start up a docker image, you can use a simple way like this:

```shell
kubevpn run deployment/authors --no-proxy
```

Example：

```shell
➜  ~ kubevpn run deployment/authors --no-proxy
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

Now the main process will hang up to show you log.

If you want to specify the image to start the container locally, you can use the parameter `--dev-image`. When the
image does not exist locally, it will be pulled from the corresponding mirror warehouse. If you want to specify startup
parameters, you can use `--entrypoint` parameter, replace it with the command you want to execute, such
as `--entrypoint /bin/bash`, for more parameters, see `kubevpn run --help`.

### DinD ( Docker in Docker ) use kubevpn in Docker

If you want to start the development mode locally using Docker in Docker (DinD), because the program will read and
write the `/tmp` directory, you need to manually add the parameter `-v /tmp:/tmp` (outer docker) and another thing is
you
need to special parameter `--network` (inner docker) for sharing network and pid

Example:

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
root@5732124e6447:/app# kubevpn run deployment/authors --headers user=naison --entrypoint sh
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
   14 root      0:02 {kubevpn} /usr/bin/qemu-x86_64 /usr/local/bin/kubevpn kubevpn run deployment/authors --headers
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

during test, check what container is running

```text
➜  ~ docker ps
CONTAINER ID   IMAGE                                  COMMAND                  CREATED         STATUS         PORTS     NAMES
1cd576b51b66   naison/authors:latest                  "sh"                     4 minutes ago   Up 4 minutes             authors_default_kubevpn_6df5f
56a6793df82d   nginx:latest                           "/docker-entrypoint.…"   4 minutes ago   Up 4 minutes             nginx_default_kubevpn_6df63
d0b3dab8912a   ghcr.io/kubenetworks/kubevpn:v2.0.0    "/bin/bash"              5 minutes ago   Up 5 minutes             upbeat_noyce
➜  ~
```

* For clean up after test

```shell
kubectl delete -f https://raw.githubusercontent.com/kubenetworks/kubevpn/master/samples/bookinfo.yaml
```

### Multiple Protocol

support OSI model layers 3 and above, protocols like `ICMP`, `TCP`, and `UDP`...

- TCP
- UDP
- ICMP
- gRPC
- Thrift
- WebSocket
- HTTP
- ...

### Cross-platform

- macOS
- Linux
- Windows

## Architecture

[architecture](https://kubevpn.dev/docs/architecture/connect).

## Contributions

Always welcome. Just opening an issue should be also grateful.

If you want to debug this project on local PC. Please follow the steps bellow:

- Startup daemon and sudo daemon process with IDE debug mode. (Essentially two GRPC server)
- Add breakpoint to file `pkg/daemon/action/connect.go:21`.
- Open another terminal run `make kubevpn`.
- Then run `./bin/kubevpn connect` and it will hit breakpoint.

### Supported by

[![JetBrains logo.](https://resources.jetbrains.com/storage/products/company/brand/logos/jetbrains.svg)](https://jb.gg/OpenSourceSupport)

### [Donate](https://kubevpn.dev/docs/donate)