FROM golang:1.25 as delve
RUN curl --location --output delve-1.25.2.tar.gz https://github.com/go-delve/delve/archive/v1.25.2.tar.gz \
  && tar xzf delve-1.25.2.tar.gz
RUN cd delve-1.25.2 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /go/dlv -ldflags '-extldflags "-static"' ./cmd/dlv/
FROM busybox
COPY --from=delve /go/dlv /bin/dlv