FROM golang:latest AS builder
WORKDIR /app
VOLUME /sys/fs/cgroup /app
RUN sed -i s/deb.debian.org/mirrors.aliyun.com/g /etc/apt/sources.list && \
    sed -i s/security.debian.org/mirrors.aliyun.com/g /etc/apt/sources.list && \
    apt update && apt-get install -y git make apt-transport-https

RUN git clone --depth=1 https://github.com/wencaiwulue/gost.git && \
 cd gost && go env -w GOPROXY=https://goproxy.cn,direct && make linux-amd64

FROM ubuntu:latest
WORKDIR /app
COPY --from=builder /usr/local/go/lib/time/zoneinfo.zip /opt/zoneinfo.zip
ENV ZONEINFO /opt/zoneinfo.zip
RUN sed -i s@/security.ubuntu.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list \
    && sed -i s@/archive.ubuntu.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list
RUN apt-get clean && apt-get update && apt-get install -y wget dnsutils vim curl net-tools iptables iputils-ping
COPY --from=0 /app/gost/bin/gost-linux-amd64 /usr/local/bin/gost