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
REGISTRY ?= docker.io
NAMESPACE ?= naison
REPOSITORY ?= kubevpn
IMAGE ?= $(REGISTRY)/$(NAMESPACE)/$(REPOSITORY):$(VERSION)
IMAGE_DEFAULT = docker.io/naison/kubevpn:latest

# Setup the -ldflags option for go build here, interpolate the variable values
LDFLAGS=--ldflags "\
 -X ${BASE}/pkg/config.Image=${IMAGE} \
 -X ${FOLDER}/cmds.Version=${VERSION} \
 -X ${FOLDER}/cmds.BuildTime=${BUILD_TIME} \
 -X ${FOLDER}/cmds.GitCommit=${GIT_COMMIT} \
 -X ${FOLDER}/cmds.Branch=${BRANCH} \
 -X ${FOLDER}/cmds.OsArch=${OS_ARCH} \
"

GO111MODULE=on
GOPROXY=https://goproxy.cn,direct

.PHONY: all
all: all-kubevpn container

.PHONY: all-kubevpn
all-kubevpn: kubevpn-darwin-amd64 kubevpn-darwin-arm64 \
kubevpn-windows-amd64 kubevpn-windows-386 kubevpn-windows-arm64 \
kubevpn-linux-amd64 kubevpn-linux-386 kubevpn-linux-arm64

.PHONY: kubevpn
kubevpn:
	make $(TARGET)

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

.PHONY: container
container:
	docker build -t ${IMAGE} -f $(BUILD_DIR)/Dockerfile .
	docker push ${IMAGE}
	docker tag ${IMAGE} ${IMAGE_DEFAULT}
	docker push ${IMAGE_DEFAULT}

############################ build local
.PHONY: container-local
container-local: kubevpn-linux-amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ./bin/envoy-xds-server ./cmd/mesh
	docker build --platform linux/amd64 -t ${IMAGE} -f $(BUILD_DIR)/local.Dockerfile .
	docker push ${IMAGE}
	docker tag ${IMAGE} ${IMAGE_DEFAULT}
	docker push ${IMAGE_DEFAULT}

