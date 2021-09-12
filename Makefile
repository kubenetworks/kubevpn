.PHONY: kubevpn-macos
kubevpn-macos:
	go build -o kubevpn ./pkg
	chmod +x kubevpn
	mv kubevpn /usr/local/bin/kubevpn

.PHONY: kubevpn-windows
kubevpn-windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o kubevpn.exe ./pkg

.PHONY: kubevpn-linux
kubevpn-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o kubevpn ./pkg
	chmod +x kubevpn
	mv kubevpn /usr/local/bin/kubevpn

.PHONY: build_image
build_image:
	docker build -t naison/kubevpn:latest -f ./remote/Dockerfile .
	docker push naison/kubevpn:latest