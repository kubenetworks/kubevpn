FROM ghcr.io/kubenetworks/kubevpn:latest

WORKDIR /app

COPY bin/kubevpn /usr/local/bin/kubevpn