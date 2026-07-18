FROM envoyproxy/envoy:v1.36.2 AS envoy
FROM debian:bookworm-slim

ARG TARGETARCH

RUN apt-get update && apt-get install -y iptables \
    && apt-get autoremove -y \
    && apt-get clean -y \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY bin/kubevpn-linux-${TARGETARCH} /usr/local/bin/kubevpn
COPY --from=envoy /usr/local/bin/envoy /usr/local/bin/envoy
