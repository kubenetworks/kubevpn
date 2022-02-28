package main

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/tun"
	"golang.org/x/net/ipv4"
	"io"
	"net"
	"sync"
)

var connsMap = &sync.Map{}

func main() {
	ip := net.ParseIP("223.254.254.100")
	listener, err := tun.Listener(tun.Config{
		Addr: ip.String() + "/24",
		MTU:  1350,
		Routes: []tun.IPRoute{{
			Dest: &net.IPNet{
				IP:   ip,
				Mask: net.CIDRMask(24, 32),
			},
			Gateway: nil,
		}},
	})
	if err != nil {
		panic(err)
	}

	tunConn, _ := listener.Accept()

	localAddr, _ := net.ResolveTCPAddr("tcp", ":1080")
	tcpListener, _ := net.ListenTCP("tcp", localAddr)

	go func() {
		for {
			bytes := make([]byte, 1000)
			n, err := tunConn.Read(bytes)
			if err != nil {
				panic(err)
			}
			go func(data []byte) {
				header, err := ipv4.ParseHeader(data)
				if err != nil {
					log.Info(err)
					return
				}
				fmt.Println(header.Src, header.Dst)
				load, ok := connsMap.Load(header.Dst.To16().String())
				if !ok {
					fmt.Println("can not found route ", header.Src, header.Dst)
					return
				}
				_, err = load.(net.Conn).Write(data)
				if err != nil {
					log.Info(err)
				}
			}(bytes[:n])
		}
	}()

	for {
		tcpConn, err := tcpListener.Accept()
		if err != nil {
			panic(err)
		}
		go func(tcpConn net.Conn) {
			defer tcpConn.Close()
			var b = make([]byte, 1000)
			n, err := tcpConn.Read(b)
			if err != nil {
				log.Info(err)
				return
			}
			header, err := ipv4.ParseHeader(b[:n])
			if err != nil {
				log.Info(err)
				return
			}
			fmt.Println(header.Src, header.Dst, "tcp server")
			connsMap.Store(header.Src.To16().String(), tcpConn)

			if _, err = tunConn.Write(b[:n]); err != nil {
				fmt.Println(err)
			}
			_, err = io.Copy(tunConn, tcpConn)
			if err != nil {
				log.Info(err)
			}
		}(tcpConn)
		//if err != nil {
		//	fmt.Println(err)
		//	continue
		//}
		//t = tcpConn
		//go func(tcpConn net.Conn) {
		//	b := make([]byte, 1000)
		//	defer tcpConn.Close()
		//	for {
		//		read, err := tcpConn.Read(b)
		//		if err != nil {
		//			fmt.Println(err)
		//			return
		//		}
		//		header, err := ipv4.ParseHeader(b[:read])
		//		if err != nil {
		//			fmt.Println(err)
		//			return
		//		}
		//		fmt.Println(header.Src, header.Dst, "tcp server")
		//		if _, err = tunConn.Write(b[:read]); err != nil {
		//			fmt.Println(err)
		//		}
		//	}
		//}(tcpConn)
	}
}
