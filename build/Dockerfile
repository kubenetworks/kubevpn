ARG BASE=github.com/wencaiwulue/kubevpn

FROM envoyproxy/envoy:v1.21.1 AS envoy
FROM golang:1.18 AS builder

COPY . /go/src/$BASE

WORKDIR /go/src/$BASE

RUN go env -w GO111MODULE=on && go env -w GOPROXY=https://goproxy.cn,direct
RUN make kubevpn-linux-amd64
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/envoy-xds-server /go/src/$BASE/cmd/mesh


FROM ubuntu:latest

RUN sed -i s@/security.ubuntu.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list \
    && sed -i s@/archive.ubuntu.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list
RUN apt-get clean && apt-get update && apt-get install -y wget dnsutils vim curl  \
    net-tools iptables iputils-ping lsof iproute2 tcpdump

WORKDIR /app

COPY --from=builder /go/src/$BASE/bin/kubevpn-linux-amd64 /usr/local/bin/kubevpn
COPY --from=builder /go/src/$BASE/bin/envoy-xds-server /bin/envoy-xds-server
COPY --from=envoy /usr/local/bin/envoy /usr/local/bin/envoy