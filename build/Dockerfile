FROM envoyproxy/envoy:v1.25.0 AS envoy
FROM golang:1.19 AS builder
ARG BASE=github.com/wencaiwulue/kubevpn

COPY . /go/src/$BASE

WORKDIR /go/src/$BASE

RUN go env -w GO111MODULE=on && go env -w GOPROXY=https://goproxy.cn,direct
RUN make kubevpn
RUN go install github.com/go-delve/delve/cmd/dlv@latest

FROM ubuntu:latest
ARG BASE=github.com/wencaiwulue/kubevpn

RUN sed -i s@/security.ubuntu.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list \
    && sed -i s@/archive.ubuntu.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list
RUN apt-get clean && apt-get update && apt-get install -y wget dnsutils vim curl  \
    net-tools iptables iputils-ping lsof iproute2 tcpdump binutils traceroute conntrack socat

ENV TZ=Asia/Shanghai \
    DEBIAN_FRONTEND=noninteractive
RUN apt update \
    && apt install -y tzdata \
    && ln -fs /usr/share/zoneinfo/${TZ} /etc/localtime \
    && echo ${TZ} > /etc/timezone \
    && dpkg-reconfigure --frontend noninteractive tzdata \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /go/src/$BASE/bin/kubevpn /usr/local/bin/kubevpn
COPY --from=builder /go/bin/dlv /usr/local/bin/dlv
COPY --from=envoy /usr/local/bin/envoy /usr/local/bin/envoy
