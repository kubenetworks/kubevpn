VERSION ?= $(shell git tag -l --sort=v:refname | tail -1)
GIT_COMMIT := $(shell git describe --match=NeVeRmAtCh --always --abbrev=40)
BUILD_TIME := $(shell date +"%Y-%m-%dT%H:%M:%SZ")
BRANCH := $(shell git rev-parse --abbrev-ref HEAD)

GOOS := $(shell go env GOHOSTOS)
GOARCH := $(shell go env GOHOSTARCH)
TARGET := kubevpn-${GOOS}-${GOARCH}
OS_ARCH := ${GOOS}/${GOARCH}

BASE := github.com/wencaiwulue/kubevpn
FOLDER := ${BASE}/cmd/kubevpn
BUILD_DIR := ./build
OUTPUT_DIR := ./bin
REGISTRY ?= naison

# Setup the -ldflags option for go build here, interpolate the variable values
LDFLAGS=--ldflags "\
 -X ${BASE}/config.Version=${VERSION} \
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
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn-darwin-amd64 ${FOLDER}
	chmod +x $(OUTPUT_DIR)/kubevpn-darwin-amd64
.PHONY: kubevpn-darwin-arm64
kubevpn-darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn-darwin-arm64 ${FOLDER}
	chmod +x $(OUTPUT_DIR)/kubevpn-darwin-arm64
# ---------darwin-----------

# ---------windows-----------
.PHONY: kubevpn-windows-amd64
kubevpn-windows-amd64:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn-windows-amd64.exe ${FOLDER}
.PHONY: kubevpn-windows-arm64
kubevpn-windows-arm64:
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn-windows-arm64.exe ${FOLDER}
.PHONY: kubevpn-windows-386
kubevpn-windows-386:
	CGO_ENABLED=0 GOOS=windows GOARCH=386 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn-windows-386.exe ${FOLDER}
# ---------windows-----------

# ---------linux-----------
.PHONY: kubevpn-linux-amd64
kubevpn-linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn-linux-amd64 ${FOLDER}
	chmod +x $(OUTPUT_DIR)/kubevpn-linux-amd64
.PHONY: kubevpn-linux-arm64
kubevpn-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn-linux-arm64 ${FOLDER}
	chmod +x $(OUTPUT_DIR)/kubevpn-linux-arm64
.PHONY: kubevpn-linux-386
kubevpn-linux-386:
	CGO_ENABLED=0 GOOS=linux GOARCH=386 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn-linux-386 ${FOLDER}
	chmod +x $(OUTPUT_DIR)/kubevpn-linux-386
# ---------linux-----------

.PHONY: image
image:
	docker build -t $(REGISTRY)/kubevpn:${VERSION} -f $(BUILD_DIR)/server/Dockerfile .
	docker tag $(REGISTRY)/kubevpn:${VERSION} $(REGISTRY)/kubevpn:latest
	docker push $(REGISTRY)/kubevpn:${VERSION}
	docker push $(REGISTRY)/kubevpn:latest

.PHONY: image-mesh
image-mesh:
	docker build -t $(REGISTRY)/kubevpn-mesh:${VERSION} -f $(BUILD_DIR)/mesh/Dockerfile .
	docker tag $(REGISTRY)/kubevpn-mesh:${VERSION} $(REGISTRY)/kubevpn-mesh:latest
	docker push $(REGISTRY)/kubevpn-mesh:${VERSION}
	docker push $(REGISTRY)/kubevpn-mesh:latest


.PHONY: image-control-plane
image-control-plane:
	docker build -t $(REGISTRY)/envoy-xds-server:${VERSION} -f $(BUILD_DIR)/control_plane/Dockerfile .
	docker tag $(REGISTRY)/envoy-xds-server:${VERSION} $(REGISTRY)/envoy-xds-server:latest
	docker push $(REGISTRY)/envoy-xds-server:${VERSION}
	docker push $(REGISTRY)/envoy-xds-server:latest

