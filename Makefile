.PHONY: kubevpn-macos
kubevpn-macos:
	go build -o kubevpn
	chmod +x kubevpn
	mv kubevpn /usr/local/bin/kubevpn

.PHONY: kubevpn-windows
kubevpn-windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o kubevpn.exe

.PHONY: kubevpn-linux
kubevpn-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o kubevpn
	chmod +x kubevpn
	mv kubevpn /usr/local/bin/kubevpn

.PHONY: image
image:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o kubevpn
	chmod +x kubevpn
	docker build -t naison/kubevpn:v2 -f ./remote/Dockerfile .
	docker push naison/kubevpn:v2
	rm -fr kubevpn.PHONY: image

.PHONY: image_mesh
image_mesh:
	docker build -t naison/kubevpnmesh:v2 -f ./remote/DockerfileMesh .
	docker push naison/kubevpnmesh:v2