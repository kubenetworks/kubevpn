package main

import (
	"io"
	"net"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/tun"
)

func main() {
	ip := net.ParseIP("223.254.254.100")
	listener, err := tun.Listener(tun.Config{
		Addr: ip.String() + "/24",
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
