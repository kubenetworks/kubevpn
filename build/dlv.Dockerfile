FROM golang:1.19 as delve
RUN curl --location --output delve-1.20.1.tar.gz https://github.com/go-delve/delve/archive/v1.20.1.tar.gz \
  && tar xzf delve-1.20.1.tar.gz
RUN cd delve-1.20.1 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /go/dlv -ldflags '-extldflags "-static"' ./cmd/dlv/
FROM busybox
WORKDIR /duct-tape
COPY --from=delve /go/dlv go/bin/