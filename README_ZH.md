# KubeVPN

[English](README.md) | [中文](README_ZH.md)

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
go build
```

如果安装了 Go 1.16 及以上版本，可以使用如下命令安装：

```shell
go install github.com/wencaiwulue/kubevpn@latest
```

## 功能

### 链接到集群网络

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

### 域名解析功能

```shell
➜  ~ kubectl get services -ntest
NAME                TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)    AGE
nginx               ClusterIP   172.20.170.170   <none>        8090/TCP   30h
tomcat              ClusterIP   172.20.203.233   <none>        8080/TCP   30h
➜  ~ curl tomcat.test.svc.cluster.local:8080
<!doctype html><html lang="en"><head><title>HTTP Status 404 – Not Found</title><style type="text/css">body {font-family:Tahoma,Arial,sans-serif;} h1, h2, h3, b {color:white;background-color:#525D76;} h1 {font-size:22px;} h2 {font-size:16px;} h3 {font-size:14px;} p {font-size:12px;} a {color:black;} .line {height:1px;background-color:#525D76;border:none;}</style></head><body><h1>HTTP Status 404 – Not Found</h1><hr class="line" /><p><b>Type</b> Status Report</p><p><b>Description</b> The origin server did not find a current representation for the target resource or is not willing to disclose that one exists.</p><hr class="line" /><h3>Apache Tomcat/9.0.52</h3></body></html>
```

### 短域名解析功能

```shell
➜  ~ kubectl get services -ntest
NAME                TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)    AGE
nginx               ClusterIP   172.20.170.170   <none>        8090/TCP   30h
tomcat              ClusterIP   172.20.203.233   <none>        8080/TCP   30h
➜  ~ curl tomcat:8080
<!doctype html><html lang="en"><head><title>HTTP Status 404 – Not Found</title><style type="text/css">body {font-family:Tahoma,Arial,sans-serif;} h1, h2, h3, b {color:white;background-color:#525D76;} h1 {font-size:22px;} h2 {font-size:16px;} h3 {font-size:14px;} p {font-size:12px;} a {color:black;} .line {height:1px;background-color:#525D76;border:none;}</style></head><body><h1>HTTP Status 404 – Not Found</h1><hr class="line" /><p><b>Type</b> Status Report</p><p><b>Description</b> The origin server did not find a current representation for the target resource or is not willing to disclose that one exists.</p><hr class="line" /><h3>Apache Tomcat/9.0.52</h3></body></html>%
```

### 反向代理

```shell
➜  ~ sudo kubevpn --namespace=test --workloads=service/nginx --workloads=service/tomcat
INFO[0000] kubeconfig path: /Users/naison/.kube/config, namespace: test, serivces: service/nginx,service/tomcat 
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

### 支持多种协议

- TCP
- UDP
- HTTP
- ICMP
- ...

### 支持三大平台

- MacOS
- Linux
- Windows

Windows 下需要安装 ```PowerShell```