package test

import (
	"fmt"
	"net"
	"testing"
	"time"
)

func TestUDP(t *testing.T) {
	go func() {
		server()
	}()
	time.Sleep(time.Second * 1)
	if err := client(); err != nil {
		t.FailNow()
	}
}

func client() error {
	socket, err := net.DialUDP("udp4", nil, &net.UDPAddr{
		IP:   net.IPv4(172, 20, 225, 47),
		Port: 55555,
	})
	if err != nil {
		fmt.Println("连接失败!", err)
		return err
	}
	defer socket.Close()

	// 发送数据
	senddata := []byte("hello server!")
	_, err = socket.Write(senddata)
	if err != nil {
		fmt.Println("发送数据失败!", err)
		return err
	}

	// 接收数据
	data := make([]byte, 4096)
	read, remoteAddr, err := socket.ReadFromUDP(data)
	if err != nil {
		fmt.Println("读取数据失败!", err)
		return err
	}
	fmt.Println(read, remoteAddr)
	fmt.Printf("%s\n", data[0:read])
	return nil
}

func server() {
	// 创建监听
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{
		IP:   net.IPv4(0, 0, 0, 0),
		Port: 55555,
	})
	if err != nil {
		return
	}
	defer socket.Close()

	for {
		data := make([]byte, 4096)
		read, remoteAddr, err := socket.ReadFromUDP(data)
		if err != nil {
			fmt.Println("读取数据失败!", err)
			continue
		}
		fmt.Println(read, remoteAddr)
		fmt.Printf("%s\n\n", data[0:read])

		senddata := []byte("hello client!")
		_, err = socket.WriteToUDP(senddata, remoteAddr)
		if err != nil {
			fmt.Println("发送数据失败!", err)
			return
		}
	}
}
