FROM envoyproxy/envoy:v1.25.0 AS envoy
FROM golang:1.23 AS builder
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
    net-tools iptables iputils-ping lsof iproute2 tcpdump binutils traceroute conntrack socat iperf3 \
    apt-transport-https ca-certificates curl

RUN if [ $(uname -m) = "x86_64" ]; then \
      echo "The architecture is AMD64"; \
      curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" && chmod +x kubectl && mv kubectl /usr/local/bin; \
    elif [ $(uname -m) = "aarch64" ]; then \
      echo "The architecture is ARM64"; \
      curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/arm64/kubectl" && chmod +x kubectl && mv kubectl /usr/local/bin; \
    else \
      echo "Unsupported architecture."; \
    fi

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
