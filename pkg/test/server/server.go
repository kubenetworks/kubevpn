package main

import (
	"io"
	"net"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
)

func main() {
	ip := net.ParseIP("fe80::cff4:d42c:7e73:e84b")
	listener, err := tun.Listener(tun.Config{
		Addr: ip.String() + "/64",
		MTU:  1350,
	})
	if err != nil {
		panic(err)
	}

	tunConn, _ := listener.Accept()

	tcpListener, err := net.Listen("tcp", ":1080")
	if err != nil {
		log.Fatal(err)
	}
	for {
		tcpConn, err := tcpListener.Accept()
		if err != nil {
			panic(err)
		}
		go func(tcpConn net.Conn) {
			defer tcpConn.Close()
			go io.Copy(tunConn, tcpConn)
			io.Copy(tcpConn, tunConn)
		}(tcpConn)
	}
}
