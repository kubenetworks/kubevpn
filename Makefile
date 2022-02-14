# These are the values we want to pass for VERSION and BUILD
VERSION := $(shell git tag -l --sort=v:refname | tail -1)
GIT_COMMIT := $(shell git describe --match=NeVeRmAtCh --always --abbrev=40)
BUILD_TIME := $(shell date +"%Y-%m-%dT%H:%M:%SZ")
BRANCH := $(shell git rev-parse --abbrev-ref HEAD)

GOOS := $(shell go env GOHOSTOS)
GOARCH := $(shell go env GOHOSTARCH)
TARGET := kubevpn-${GOOS}-${GOARCH}
OS_ARCH := ${GOOS}/${GOARCH}

# Setup the -ldflags option for go build here, interpolate the variable values
LDFLAGS=--ldflags "-w -s \
 -X github.com/wencaiwulue/kubevpn/cmd/kubevpn/cmds.Version=${VERSION} \
 -X github.com/wencaiwulue/kubevpn/cmd/kubevpn/cmds.BuildTime=${BUILD_TIME} \
 -X github.com/wencaiwulue/kubevpn/cmd/kubevpn/cmds.GitCommit=${GIT_COMMIT} \
 -X github.com/wencaiwulue/kubevpn/cmd/kubevpn/cmds.Branch=${BRANCH} \
 -X github.com/wencaiwulue/kubevpn/cmd/kubevpn/cmds.OsArch=${OS_ARCH} \
"

.PHONY: kubevpn-macos
kubevpn-macos:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ${LDFLAGS} -o kubevpn-darwin-amd64 github.com/wencaiwulue/kubevpn/cmd/kubevpn
	chmod +x kubevpn-darwin-amd64
	cp kubevpn-darwin-amd64 /usr/local/bin/kubevpn

.PHONY: kubevpn-windows
kubevpn-windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ${LDFLAGS} -o kubevpn-windows-amd64.exe github.com/wencaiwulue/kubevpn/cmd/kubevpn

.PHONY: kubevpn-linux
kubevpn-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -o kubevpn-linux-amd64 github.com/wencaiwulue/kubevpn/cmd/kubevpn
	chmod +x kubevpn-linux-amd64
	cp kubevpn-linux-amd64 /usr/local/bin/kubevpn

.PHONY: control-plane-linux
control-plane-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o envoy-xds-server github.com/wencaiwulue/kubevpn/pkg/controlplane/cmd/server
	chmod +x envoy-xds-server

.PHONY: image
image: kubevpn-linux
	mv kubevpn-linux-amd64 kubevpn
	docker build -t naison/kubevpn:v2 -f ./dockerfile/server/Dockerfile .
	rm -fr kubevpn
	docker push naison/kubevpn:v2

.PHONY: image_mesh
image_mesh:
	docker build -t naison/kubevpnmesh:v2 -f ./dockerfile/mesh/Dockerfile .
	docker push naison/kubevpnmesh:v2

.PHONY: image_control_plane
image_control_plane: control-plane-linux
	docker build -t naison/envoy-xds-server:latest -f ./dockerfile/controlplane/Dockerfile .
	rm -fr envoy-xds-server
	docker push naison/envoy-xds-server:latest

