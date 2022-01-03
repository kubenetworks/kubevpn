FROM envoyproxy/envoy-dev:5f7d6efb5786ee3de31b1fb37c78fa281718b704

WORKDIR /app
RUN sed -i s@/security.ubuntu.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list \
    && sed -i s@/archive.ubuntu.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list
RUN apt-get clean && apt-get update && apt-get install -y wget dnsutils vim curl net-tools iptables iputils-ping lsof iproute2