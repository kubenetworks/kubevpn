FROM envoyproxy/envoy:v1.25.0 AS envoy
FROM ubuntu:latest

RUN sed -i s@/security.ubuntu.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list \
    && sed -i s@/archive.ubuntu.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list
RUN apt-get clean && apt-get update && apt-get install -y wget dnsutils vim curl  \
    net-tools iptables iputils-ping lsof iproute2 tcpdump

WORKDIR /app

COPY bin/kubevpn /usr/local/bin/kubevpn
COPY --from=envoy /usr/local/bin/envoy /usr/local/bin/envoy