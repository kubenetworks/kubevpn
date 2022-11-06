FROM envoyproxy/envoy:v1.21.1 AS envoy
FROM ubuntu:latest

RUN sed -i s@/security.ubuntu.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list \
    && sed -i s@/archive.ubuntu.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list
RUN apt-get clean && apt-get update && apt-get install -y wget dnsutils vim curl  \
    net-tools iptables iputils-ping lsof iproute2 tcpdump

WORKDIR /app

COPY bin/kubevpn-linux-amd64 /usr/local/bin/kubevpn
COPY bin/envoy-xds-server /bin/envoy-xds-server
COPY --from=envoy /usr/local/bin/envoy /usr/local/bin/envoy