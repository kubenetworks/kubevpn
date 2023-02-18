package main

import (
	"io"
	"net"

	"github.com/containernetworking/cni/pkg/types"
	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/tun"
)

func main() {
	ip := net.ParseIP("223.254.254.102")
	listener, err := tun.Listener(tun.Config{
		Addr: ip.String() + "/24",
		MTU:  1350,
		Routes: []types.Route{{
			Dst: net.IPNet{
				IP:   ip,
				Mask: net.CIDRMask(24, 32),
			},
		}, {
			Dst: net.IPNet{
				IP:   net.ParseIP("192.168.0.0"),
				Mask: net.CIDRMask(24, 32),
			},
		}},
	})
	if err != nil {
		panic(err)
	}
	tunConn, err := listener.Accept()
	defer tunConn.Close()
	tcpConn, err := net.Dial("tcp", ":1080")
	if err != nil {
		log.Fatal(err)
	}
	go io.Copy(tunConn, tcpConn)
	io.Copy(tcpConn, tunConn)
}
