.PHONY: vpn
vpn:
	go build -o vpn ./pkg && chmod +x vpn

.PHONY: build_image
build_image:
	docker build -t naison/kubevpn:latest -f ./remote/Dockerfile .