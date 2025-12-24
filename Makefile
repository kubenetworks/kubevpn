VERSION ?= $(shell git tag -l --sort=v:refname | tail -1)
GIT_COMMIT ?= $(shell git describe --match=NeVeRmAtCh --always --abbrev=7)
BUILD_TIME ?= $(shell date +"%Y-%m-%dT%H:%M:%SZ")
BRANCH ?= $(shell git rev-parse --abbrev-ref HEAD)

GOOS := $(shell go env GOHOSTOS)
GOARCH := $(shell go env GOHOSTARCH)
TARGET := kubevpn-${GOOS}-${GOARCH}
OS_ARCH := ${GOOS}/${GOARCH}

BASE := github.com/wencaiwulue/kubevpn/v2
FOLDER := ${BASE}/cmd/kubevpn
BUILD_DIR ?= ./build
OUTPUT_DIR ?= ./bin
REGISTRY ?= docker.io
NAMESPACE ?= naison
REPOSITORY ?= kubevpn
IMAGE ?= $(REGISTRY)/$(NAMESPACE)/$(REPOSITORY):$(VERSION)
IMAGE_LATEST ?= docker.io/naison/kubevpn:latest
IMAGE_GH ?= ghcr.io/kubenetworks/kubevpn:$(VERSION)
IMAGE_GH_LATEST ?= ghcr.io/kubenetworks/kubevpn:latest

# Setup the -ldflags option for go build here, interpolate the variable values
LDFLAGS=--ldflags "-s -w\
 -X ${BASE}/pkg/config.Image=${IMAGE_GH} \
 -X ${BASE}/pkg/config.Version=${VERSION} \
 -X ${BASE}/pkg/config.GitCommit=${GIT_COMMIT} \
 -X ${BASE}/pkg/config.GitHubOAuthToken=${GitHubOAuthToken} \
 -X ${FOLDER}/cmds.BuildTime=${BUILD_TIME} \
 -X ${FOLDER}/cmds.Branch=${BRANCH} \
 -X ${FOLDER}/cmds.OsArch=${OS_ARCH} \
"

GO111MODULE=on
GOPROXY=https://goproxy.cn,direct

.PHONY: all
all: kubevpn-all container

.PHONY: kubevpn-all
kubevpn-all: kubevpn-darwin-amd64 kubevpn-darwin-arm64 \
kubevpn-windows-amd64 kubevpn-windows-386 kubevpn-windows-arm64 \
kubevpn-linux-amd64 kubevpn-linux-386 kubevpn-linux-arm64

.PHONY: kubevpn
kubevpn:
	make $(TARGET)

# ---------darwin-----------
.PHONY: kubevpn-darwin-amd64
kubevpn-darwin-amd64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn ${FOLDER}
	chmod +x $(OUTPUT_DIR)/kubevpn
.PHONY: kubevpn-darwin-arm64
kubevpn-darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn ${FOLDER}
	chmod +x $(OUTPUT_DIR)/kubevpn
# ---------darwin-----------

# ---------windows-----------
.PHONY: kubevpn-windows-amd64
kubevpn-windows-amd64:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn.exe ${FOLDER}
.PHONY: kubevpn-windows-arm64
kubevpn-windows-arm64:
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn.exe ${FOLDER}
.PHONY: kubevpn-windows-386
kubevpn-windows-386:
	CGO_ENABLED=0 GOOS=windows GOARCH=386 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn.exe ${FOLDER}
# ---------windows-----------

# ---------linux-----------
.PHONY: kubevpn-linux-amd64
kubevpn-linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn ${FOLDER}
	chmod +x $(OUTPUT_DIR)/kubevpn
.PHONY: kubevpn-linux-arm64
kubevpn-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn ${FOLDER}
	chmod +x $(OUTPUT_DIR)/kubevpn
.PHONY: kubevpn-linux-386
kubevpn-linux-386:
	CGO_ENABLED=0 GOOS=linux GOARCH=386 go build ${LDFLAGS} -o $(OUTPUT_DIR)/kubevpn ${FOLDER}
	chmod +x $(OUTPUT_DIR)/kubevpn
# ---------linux-----------

.PHONY: container
container:
	docker buildx build --platform linux/amd64,linux/arm64 -t ${IMAGE} -t ${IMAGE_LATEST} -t ${IMAGE_GH} -t ${IMAGE_GH_LATEST} -f $(BUILD_DIR)/Dockerfile --push .

############################ build local
.PHONY: container-local
container-local: kubevpn-linux-amd64
	docker buildx build --platform linux/amd64,linux/arm64 -t ${IMAGE_LATEST} -t ${IMAGE_GH_LATEST} -f $(BUILD_DIR)/local.Dockerfile --push .

.PHONY: container-test
container-test: kubevpn-linux-amd64
	docker build -t ${IMAGE_GH} -f $(BUILD_DIR)/test.Dockerfile --push .

.PHONY: version
version:
	go run ${BASE}/pkg/util/krew

.PHONY: gen
gen:
	go generate ./...

.PHONY: ut
ut:
	go test -p=1 -v -timeout=120m -coverprofile=coverage.txt -coverpkg=./... ./...

.PHONY: cover
cover: ut
	go tool cover -html=coverage.txt