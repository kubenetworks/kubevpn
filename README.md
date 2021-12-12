# KubeVPN

[中文](README_ZH.md) | [English](README.md)

A tools which can connect to kubernetes cluster network, you can access remote kubernetes cluster network, remote
kubernetes cluster service can also access your local service

## QuickStart

```shell
git clone https://github.com/wencaiwulue/kubevpn.git
cd kubevpn
make kubevpn-linux
make kubevpn-macos
make kubevpn-windows
```

if you are using windows, you can build by this command:

```shell
go build github.com/wencaiwulue/kubevpn/cmd/kubevpn
```

if you installed Go 1.16+, you can use install it by this command directly:

```shell
go install github.com/wencaiwulue/kubevpn/cmd/kubevpn@latest
```

### Install bookinfo as demo application

```shell
kubectl apply -f samples/bookinfo/platform/kube/bookinfo.yaml
```

## Functions

### Connect to k8s cluster network

```shell
sudo kubevpn connect --namespace=test
Password:
INFO[0000] kubeconfig path: /Users/naison/.kube/config, namespace: test, serivces:  
INFO[0001] update ref count successfully                
INFO[0001] your ip is 223.254.254.38/24                 
Forwarding from 127.0.0.1:10800 -> 10800
Forwarding from [::1]:10800 -> 10800
INFO[0005] port forward ready 
...
```

```shell
➜  ~ kubectl get pods -n test -owide
NAME                                        READY   STATUS      RESTARTS   AGE     IP             NODE         NOMINATED NODE   READINESS GATES
kubevpn.traffic.manager                     1/1     Running     0          6h39m   172.20.0.252   172.30.0.7   <none>           <none>
nginx-886bbf7c9-vgqbr                       1/1     Running     0          55m     172.20.0.67    172.30.0.7   <none>           <none>
tomcat-7449544d95-ztklj                     1/1     Running     0          55m     172.20.0.65    172.30.0.7   <none>           <none>
```

```shell
➜  ~ ping 172.20.0.65
PING 172.20.0.65 (172.20.0.65): 56 data bytes
64 bytes from 172.20.0.65: icmp_seq=0 ttl=63 time=38.791 ms
64 bytes from 172.20.0.65: icmp_seq=1 ttl=63 time=38.853 ms
64 bytes from 172.20.0.65: icmp_seq=2 ttl=63 time=39.199 ms
64 bytes from 172.20.0.65: icmp_seq=3 ttl=63 time=38.667 ms
^C
--- 172.20.0.65 ping statistics ---
4 packets transmitted, 4 packets received, 0.0% packet loss
round-trip min/avg/max/stddev = 38.667/38.877/39.199/0.197 ms
```

```shell
➜  ~ kubectl get services -ntest  -o wide
NAME                TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)    AGE     SELECTOR
nginx               ClusterIP   172.20.170.170   <none>        8090/TCP   30h     app=nginx
tomcat              ClusterIP   172.20.203.233   <none>        8080/TCP   30h     app=tomcat
➜  ~ curl 172.20.203.233:8080
<!doctype html><html lang="en"><head><title>HTTP Status 404 – Not Found</title><style type="text/css">body {font-family:Tahoma,Arial,sans-serif;} h1, h2, h3, b {color:white;background-color:#525D76;} h1 {font-size:22px;} h2 {font-size:16px;} h3 {font-size:14px;} p {font-size:12px;} a {color:black;} .line {height:1px;background-color:#525D76;border:none;}</style></head><body><h1>HTTP Status 404 – Not Found</h1><hr class="line" /><p><b>Type</b> Status Report</p><p><b>Description</b> The origin server did not find a current representation for the target resource or is not willing to disclose that one exists.</p><hr class="line" /><h3>Apache Tomcat/9.0.52</h3></body></html>
```

### Domain resolve

```shell
➜  ~ kubectl get services -ntest
NAME                TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)    AGE
nginx               ClusterIP   172.20.170.170   <none>        8090/TCP   30h
tomcat              ClusterIP   172.20.203.233   <none>        8080/TCP   30h
➜  ~ curl tomcat.test.svc.cluster.local:8080
<!doctype html><html lang="en"><head><title>HTTP Status 404 – Not Found</title><style type="text/css">body {font-family:Tahoma,Arial,sans-serif;} h1, h2, h3, b {color:white;background-color:#525D76;} h1 {font-size:22px;} h2 {font-size:16px;} h3 {font-size:14px;} p {font-size:12px;} a {color:black;} .line {height:1px;background-color:#525D76;border:none;}</style></head><body><h1>HTTP Status 404 – Not Found</h1><hr class="line" /><p><b>Type</b> Status Report</p><p><b>Description</b> The origin server did not find a current representation for the target resource or is not willing to disclose that one exists.</p><hr class="line" /><h3>Apache Tomcat/9.0.52</h3></body></html>
```

### Short domain resolve

```shell
➜  ~ kubectl get services -ntest
NAME                TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)    AGE
nginx               ClusterIP   172.20.170.170   <none>        8090/TCP   30h
tomcat              ClusterIP   172.20.203.233   <none>        8080/TCP   30h
➜  ~ curl tomcat:8080
<!doctype html><html lang="en"><head><title>HTTP Status 404 – Not Found</title><style type="text/css">body {font-family:Tahoma,Arial,sans-serif;} h1, h2, h3, b {color:white;background-color:#525D76;} h1 {font-size:22px;} h2 {font-size:16px;} h3 {font-size:14px;} p {font-size:12px;} a {color:black;} .line {height:1px;background-color:#525D76;border:none;}</style></head><body><h1>HTTP Status 404 – Not Found</h1><hr class="line" /><p><b>Type</b> Status Report</p><p><b>Description</b> The origin server did not find a current representation for the target resource or is not willing to disclose that one exists.</p><hr class="line" /><h3>Apache Tomcat/9.0.52</h3></body></html>%
```

### Reverse proxy

```shell
➜  ~ sudo kubevpn connect --namespace=test --workloads=service/nginx --workloads=serivce/tomcat
INFO[0000] kubeconfig path: /Users/naison/.kube/config, namespace: test, serivces: serivce/nginx,service/tomcat 
INFO[0001] prepare to expose local service to remote service: tomcat 
INFO[0001] prepare to expose local service to remote service: nginx 
INFO[0032] your ip is 223.254.254.43/24                 
Forwarding from 127.0.0.1:10800 -> 10800
Forwarding from [::1]:10800 -> 10800
INFO[0036] port forward ready 
...
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
	_ = http.ListenAndServe(":8080", nil)
}
```

```shell
➜  ~ curl tomcat:8080
Hello world!%
➜  ~ curl tomcat.test.svc.cluster.local:8080
Hello world!%
```

### Reverse proxy with mesh

Only support HTTP and GRPC, with specific header `"KubeVPN-Routing-Tag: kubevpn"` will route to your local machine

```shell
➜  ~ sudo kubevpn connect --namespace=test --workloads=service/nginx --workloads=serivce/tomcat --mode=mesh
INFO[0000] kubeconfig path: /Users/naison/.kube/config, namespace: test, serivces: serivce/nginx,service/tomcat 
INFO[0001] prepare to expose local service to remote service: tomcat 
INFO[0001] prepare to expose local service to remote service: nginx 
INFO[0032] your ip is 223.254.254.43/24                 
Forwarding from 127.0.0.1:10800 -> 10800
Forwarding from [::1]:10800 -> 10800
INFO[0036] port forward ready 
...
```

```shell
➜  ~ curl tomcat:8080
<!doctype html><html lang="en"><head><title>HTTP Status 404 – Not Found</title><style type="text/css">body {font-family:Tahoma,Arial,sans-serif;} h1, h2, h3, b {color:white;background-color:#525D76;} h1 {font-size:22px;} h2 {font-size:16px;} h3 {font-size:14px;} p {font-size:12px;} a {color:black;} .line {height:1px;background-color:#525D76;border:none;}</style></head><body><h1>HTTP Status 404 – Not Found</h1><hr class="line" /><p><b>Type</b> Status Report</p><p><b>Description</b> The origin server did not find a current representation for the target resource or is not willing to disclose that one exists.</p><hr class="line" /><h3>Apache Tomcat/9.0.52</h3></body></html>%
➜  ~ curl tomcat:8080 -H "KubeVPN-Routing-Tag: kubevpn"
Hello world!%
```

### Multiple Protocol

- TCP
- UDP
- HTTP
- ICMP
- ...

### Cross-platform

- MacOS
- Linux
- Windows

on Windows platform, you need to install ```PowerShell``` in advance