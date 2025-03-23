FROM envoyproxy/envoy:v1.25.0 AS envoy
FROM golang:1.23 AS builder
ARG BASE=github.com/wencaiwulue/kubevpn

COPY . /go/src/$BASE

WORKDIR /go/src/$BASE

RUN make kubevpn

FROM debian:bookworm-slim
ARG BASE=github.com/wencaiwulue/kubevpn

RUN apt-get update && apt-get install -y iptables curl \
    && if [ $(uname -m) = "x86_64" ]; then \
         echo "The architecture is AMD64"; \
         curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"; \
       elif [ $(uname -m) = "aarch64" ]; then \
         echo "The architecture is ARM64"; \
         curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/arm64/kubectl"; \
       else \
         echo "Unsupported architecture."; \
         exit 1; \
       fi \
    && chmod +x kubectl && mv kubectl /usr/local/bin \
    && apt-get remove -y curl \
    && apt-get autoremove -y \
    && apt-get clean -y \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /go/src/$BASE/bin/kubevpn /usr/local/bin/kubevpn
COPY --from=envoy /usr/local/bin/envoy /usr/local/bin/envoy
