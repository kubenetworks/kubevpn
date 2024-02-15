FROM naison/kubevpn:latest

WORKDIR /app

RUN if [ $(uname -m) = "x86_64" ]; then \
      echo "The architecture is AMD64"; \
      curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" && chmod +x kubectl && mv kubectl /usr/local/bin; \
    elif [ $(uname -m) = "aarch64" ]; then \
      echo "The architecture is ARM64"; \
      curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/arm64/kubectl" && chmod +x kubectl && mv kubectl /usr/local/bin; \
    else \
      echo "Unsupported architecture."; \
    fi

COPY bin/kubevpn /usr/local/bin/kubevpn