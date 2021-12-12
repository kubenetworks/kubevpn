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

.PHONY: image
image: kubevpn-linux
	docker build -t naison/kubevpn:v2 -f ./dockerfile/Dockerfile .
	docker push naison/kubevpn:v2

.PHONY: image_mesh
image_mesh:
	docker build -t naison/kubevpnmesh:v2 -f ./dockerfile/DockerfileMesh .
	docker push naison/kubevpnmesh:v2