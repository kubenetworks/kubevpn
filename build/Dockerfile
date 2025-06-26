FROM envoyproxy/envoy:v1.34.1 AS envoy
FROM golang:1.23 AS builder
ARG BASE=github.com/wencaiwulue/kubevpn

COPY . /go/src/$BASE

WORKDIR /go/src/$BASE

RUN make kubevpn

FROM debian:bookworm-slim
ARG BASE=github.com/wencaiwulue/kubevpn

RUN apt-get update && apt-get install -y iptables dnsutils \
    && apt-get autoremove -y \
    && apt-get clean -y \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /go/src/$BASE/bin/kubevpn /usr/local/bin/kubevpn
COPY --from=envoy /usr/local/bin/envoy /usr/local/bin/envoy
