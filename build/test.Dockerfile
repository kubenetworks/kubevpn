FROM naison/kubevpn:latest

WORKDIR /app

RUN apt-get clean && apt-get update && apt-get install -y iperf3

COPY bin/kubevpn /usr/local/bin/kubevpn