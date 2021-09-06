.PHONY: vpn
vpn:
	go build -o vpn ./pkg
	chmod +x vpn
	mv vpn /usr/local/bin/kubevpn

.PHONY: build_image
build_image:
	docker build -t naison/kubevpn:latest -f ./remote/Dockerfile .
	docker push naison/kubevpn:latest