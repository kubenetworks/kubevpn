.PHONY: kubevpn-macos
kubevpn-macos:
	go build -o kubevpn github.com/wencaiwulue/kubevpn/cmd/kubevpn
	chmod +x kubevpn
	cp kubevpn /usr/local/bin/kubevpn

.PHONY: kubevpn-windows
kubevpn-windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o kubevpn.exe github.com/wencaiwulue/kubevpn/cmd/kubevpn

.PHONY: kubevpn-linux
kubevpn-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o kubevpn github.com/wencaiwulue/kubevpn/cmd/kubevpn
	chmod +x kubevpn
	cp kubevpn /usr/local/bin/kubevpn

.PHONY: control-plane-linux
control-plane-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o envoy-xds-server github.com/wencaiwulue/kubevpn/pkg/control_plane/cmd/server
	chmod +x envoy-xds-server

.PHONY: image
image: kubevpn-linux
	docker build -t naison/kubevpn:v2 -f ./dockerfile/server/Dockerfile .
	rm -fr kubevpn
	docker push naison/kubevpn:v2

.PHONY: image_mesh
image_mesh:
	docker build -t naison/kubevpnmesh:v2 -f ./dockerfile/mesh/Dockerfile .
	docker push naison/kubevpnmesh:v2

.PHONY: image_control_plane
image_control_plane: control-plane-linux
	docker build -t naison/envoy-xds-server:latest -f ./dockerfile/control_plane/Dockerfile .
	rm -fr envoy-xds-server
	docker push naison/envoy-xds-server:latest

