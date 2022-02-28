# These are the values we want to pass for VERSION and BUILD
VERSION := $(shell git tag -l --sort=v:refname | tail -1)
GIT_COMMIT := $(shell git describe --match=NeVeRmAtCh --always --abbrev=40)
BUILD_TIME := $(shell date +"%Y-%m-%dT%H:%M:%SZ")
BRANCH := $(shell git rev-parse --abbrev-ref HEAD)

GOOS := $(shell go env GOHOSTOS)
GOARCH := $(shell go env GOHOSTARCH)
TARGET := kubevpn-${GOOS}-${GOARCH}
OS_ARCH := ${GOOS}/${GOARCH}

FOLDER := github.com/wencaiwulue/kubevpn/cmd/kubevpn
CONTROL_PLANE_FOLDER := github.com/wencaiwulue/kubevpn/pkg/controlplane/cmd/server

# Setup the -ldflags option for go build here, interpolate the variable values
LDFLAGS=--ldflags "\
 -X ${FOLDER}/cmds.Version=${VERSION} \
 -X ${FOLDER}/cmds.BuildTime=${BUILD_TIME} \
 -X ${FOLDER}/cmds.GitCommit=${GIT_COMMIT} \
 -X ${FOLDER}/cmds.Branch=${BRANCH} \
 -X ${FOLDER}/cmds.OsArch=${OS_ARCH} \
"

.PHONY: all
all: all-kubevpn all-image

.PHONY: all-kubevpn
all-kubevpn: kubevpn-darwin-amd64 kubevpn-darwin-arm64 \
kubevpn-windows-amd64 kubevpn-windows-386 kubevpn-windows-arm64 \
kubevpn-linux-amd64 kubevpn-linux-386 kubevpn-linux-arm64

.PHONY: all-image
all-image: image image-mesh image-control-plane

# ---------darwin-----------
.PHONY: kubevpn-darwin-amd64
kubevpn-darwin-amd64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ${LDFLAGS} -o kubevpn-darwin-amd64 ${FOLDER}
	chmod +x kubevpn-darwin-amd64
	cp kubevpn-darwin-amd64 /usr/local/bin/kubevpn
.PHONY: kubevpn-darwin-arm64
kubevpn-darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ${LDFLAGS} -o kubevpn-darwin-arm64 ${FOLDER}
	chmod +x kubevpn-darwin-arm64
	cp kubevpn-darwin-arm64 /usr/local/bin/kubevpn
# ---------darwin-----------

# ---------windows-----------
.PHONY: kubevpn-windows-amd64
kubevpn-windows-amd64:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ${LDFLAGS} -o kubevpn-windows-amd64.exe ${FOLDER}
.PHONY: kubevpn-windows-arm64
kubevpn-windows-arm64:
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ${LDFLAGS} -o kubevpn-windows-arm64.exe ${FOLDER}
.PHONY: kubevpn-windows-386
kubevpn-windows-386:
	CGO_ENABLED=0 GOOS=windows GOARCH=386 go build ${LDFLAGS} -o kubevpn-windows-386.exe ${FOLDER}
# ---------windows-----------

# ---------linux-----------
.PHONY: kubevpn-linux-amd64
kubevpn-linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -o kubevpn-linux-amd64 ${FOLDER}
	chmod +x kubevpn-linux-amd64
	cp kubevpn-linux-amd64 /usr/local/bin/kubevpn
.PHONY: kubevpn-linux-arm64
kubevpn-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ${LDFLAGS} -o kubevpn-linux-arm64 ${FOLDER}
	chmod +x kubevpn-linux-arm64
	cp kubevpn-linux-arm64 /usr/local/bin/kubevpn
.PHONY: kubevpn-linux-386
kubevpn-linux-386:
	CGO_ENABLED=0 GOOS=linux GOARCH=386 go build ${LDFLAGS} -o kubevpn-linux-386 ${FOLDER}
	chmod +x kubevpn-linux-386
	cp kubevpn-linux-386 /usr/local/bin/kubevpn
# ---------linux-----------

.PHONY: image
image: kubevpn-linux-amd64
	mv kubevpn-linux-amd64 kubevpn
	docker build -t naison/kubevpn:v2 -f ./dockerfile/server/Dockerfile .
	rm -fr kubevpn
	docker push naison/kubevpn:v2

.PHONY: image-mesh
image-mesh:
	docker build -t naison/kubevpnmesh:v2 -f ./dockerfile/mesh/Dockerfile .
	docker push naison/kubevpnmesh:v2

.PHONY: image-control-plane
image-control-plane:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o envoy-xds-server ${CONTROL_PLANE_FOLDER}
	chmod +x envoy-xds-server
	docker build -t naison/envoy-xds-server:latest -f ./dockerfile/controlplane/Dockerfile .
	rm -fr envoy-xds-server
	docker push naison/envoy-xds-server:latest

