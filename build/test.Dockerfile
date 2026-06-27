FROM envoyproxy/envoy:v1.36.2 AS envoy
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y iptables dnsutils \
    && apt-get autoremove -y \
    && apt-get clean -y \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY bin/kubevpn /usr/local/bin/kubevpn
COPY --from=envoy /usr/local/bin/envoy /usr/local/bin/envoy
