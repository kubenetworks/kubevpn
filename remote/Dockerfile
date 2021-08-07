FROM ubuntu:latest
WORKDIR /app
RUN sed -i s@/archive.ubuntu.com/@/mirrors.aliyun.com/@g /etc/apt/sources.list
RUN apt-get clean && apt-get update && apt-get install -y wget dnsutils vim curl net-tools iptables iputils-ping

RUN curl -fL https://github.com/ginuerzh/gost/releases/download/v2.11.1/gost-linux-amd64-2.11.1.gz -o gost.gz
RUN gzip gost.gz -d && chmod +x gost && mv gost /usr/local/bin/gost