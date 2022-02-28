package main

import (
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/tun"
	"io"
	"net"
)

func main() {
	ip := net.ParseIP("223.254.254.102")
	listener, err := tun.Listener(tun.Config{
		Addr: ip.String() + "/24",
		MTU:  1350,
		Routes: []tun.IPRoute{{
			Dest: &net.IPNet{
				IP:   ip,
				Mask: net.CIDRMask(24, 32),
			},
		}, {
			Dest: &net.IPNet{
				IP:   net.ParseIP("172.16.0.0"),
				Mask: net.CIDRMask(16, 32),
			},
		}},
	})
	if err != nil {
		panic(err)
	}

	//bytes := make([]byte, 1000)
	tunConn, err := listener.Accept()
	defer tunConn.Close()
	addr, _ := net.ResolveTCPAddr("tcp", ":1080")
	tcp, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		_, err := io.Copy(tunConn, tcp)
		if err != nil {
			log.Info(err)
		}
	}()
	_, err = io.Copy(tcp, tunConn)
	if err != nil {
		log.Info(err)
	}
	//go func() {
	//	res := make([]byte, 100)
	//	defer tcp.Close()
	//	for {
	//		i, err := tcp.Read(res)
	//		if err != nil {
	//			fmt.Println(err)
	//			return
	//		}
	//		if _, err = tunConn.Write(res[:i]); err != nil {
	//			fmt.Println(err)
	//		}
	//	}
	//}()
	//for {
	//	read, err := tunConn.Read(bytes)
	//	if err != nil {
	//		panic(err)
	//	}
	//	fmt.Printf("tun local: %v, tun rmeote: %v\n", tunConn.LocalAddr(), tunConn.RemoteAddr())
	//	header, err := ipv4.ParseHeader(bytes[:read])
	//	if err != nil {
	//		panic(err)
	//	}
	//	fmt.Printf("src: %v, dst: %v\n", header.Src, header.Dst)
	//	// port-forward to 10800
	//	if header.Dst.Equal(ip) {
	//		_, err = tunConn.Write(bytes[:read])
	//		if err != nil {
	//			fmt.Println(err)
	//		}
	//	} else {
	//		fmt.Println("forward it to remote")
	//		_, err = tcp.Write(bytes[:read])
	//		if err != nil && strings.Contains(err.Error(), "use of closed network connection") {
	//			tcp, err = net.DialTCP("tcp", nil, addr)
	//		}
	//	}
	//}
}
