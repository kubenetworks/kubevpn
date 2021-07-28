module kubevpn

go 1.16

require (
	git.torproject.org/pluggable-transports/goptlib.git v1.1.0
	git.torproject.org/pluggable-transports/obfs4.git v0.0.0-20201207231651-40245c4a1cf2
	github.com/LiamHaworth/go-tproxy v0.0.0-20190726054950-ef7efd7f24ed
	github.com/aead/chacha20 v0.0.0-20180709150244-8b13a72661da // indirect
	github.com/asaskevich/govalidator v0.0.0-20190424111038-f61b66f89f4a
	github.com/coreos/go-iptables v0.6.0 // indirect
	github.com/docker/libcontainer v2.2.1+incompatible
	github.com/ginuerzh/gosocks4 v0.0.1
	github.com/ginuerzh/gosocks5 v0.2.0
	github.com/ginuerzh/tls-dissector v0.0.1
	github.com/go-gost/relay v0.1.0
	github.com/go-log/log v0.2.0
	github.com/gobwas/glob v0.2.3
	github.com/google/gopacket v1.1.19 // indirect
	github.com/gorilla/websocket v1.4.2
	github.com/klauspost/compress v1.4.1
	github.com/klauspost/reedsolomon v1.9.12 // indirect
	github.com/lucas-clemente/quic-go v0.22.0
	github.com/miekg/dns v1.0.14
	github.com/milosgajdos/tenus v0.0.3
	github.com/moby/term v0.0.0-20210619224110-3f7ff695adc6
	github.com/pkg/errors v0.9.1
	github.com/ryanuber/go-glob v1.0.0
	github.com/shadowsocks/go-shadowsocks2 v0.1.5
	github.com/shadowsocks/shadowsocks-go v0.0.0-20200409064450-3e585ff90601
	github.com/sirupsen/logrus v1.8.1
	github.com/songgao/water v0.0.0-20200317203138-2b4b6d7c09d8
	github.com/templexxx/cpufeat v0.0.0-20180724012125-cef66df7f161 // indirect
	github.com/templexxx/xor v0.0.0-20191217153810-f85b25db303b // indirect
	github.com/tjfoc/gmsm v1.4.1 // indirect
	github.com/xtaci/kcp-go v5.4.20+incompatible
	github.com/xtaci/lossyconn v0.0.0-20200209145036-adba10fffc37 // indirect
	github.com/xtaci/smux v1.5.15
	github.com/xtaci/tcpraw v1.2.25
	gitlab.com/yawning/obfs4.git v0.0.0-00010101000000-000000000000 // indirect
	golang.org/x/crypto v0.0.0-20210503195802-e9a32991a82e
	golang.org/x/net v0.0.0-20210504132125-bbd867fde50d
	k8s.io/api v0.21.2
	k8s.io/apimachinery v0.21.2
	k8s.io/cli-runtime v0.21.2
	k8s.io/client-go v0.21.2
	k8s.io/kubectl v0.21.2
)

replace (
	git.torproject.org/pluggable-transports/obfs4.git v0.0.0-20201207231651-40245c4a1cf2 => gitlab.com/yawning/obfs4.git v0.0.0-20210511220700-e330d1b7024b
	gitlab.com/yawning/obfs4.git => git.torproject.org/pluggable-transports/obfs4.git v0.0.0-20201207231651-40245c4a1cf2
)
