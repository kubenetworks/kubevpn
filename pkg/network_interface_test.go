package pkg

import (
	"fmt"
	"net"
	"syscall"
	"testing"

	"golang.org/x/net/route"
)

// netstat -anr | grep 10.61
//10.61/16 utun4 USc utun4
//10.61.10.251 10.176.32.40 UGHWIig utun2
//10.61.64/18 10.61.64.1 UCS utun3
//10.61.64.1 10.61.64.0 UH utun3
func TestConflict(t *testing.T) {
	var mm = make(map[string][]*net.IPNet)
	_, ipNet, err := net.ParseCIDR("10.61.0.0/16")
	if err != nil {
		panic(err)
	}
	_, ipNet2, err := net.ParseCIDR("10.61.64.0/18")
	if err != nil {
		panic(err)
	}
	_, ipNet3, err := net.ParseCIDR("10.61.64.1/24")
	if err != nil {
		panic(err)
	}

	mm["utun4"] = []*net.IPNet{ipNet}
	mm["utun3"] = []*net.IPNet{ipNet2, ipNet3}

	var origin = "utun4"
	conflict := detectConflictDevice(origin, mm)
	fmt.Println(conflict)
}

func TestGetConflictDevice(t *testing.T) {
	err := DetectAndDisableConflictDevice("utun2")
	if err != nil {
		panic(err)
	}
}

func TestParse(t *testing.T) {
	rib, err := route.FetchRIB(syscall.AF_INET, syscall.NET_RT_DUMP, 0)
	if err != nil {
		panic(err)
	}
	msgs, err := route.ParseRIB(syscall.NET_RT_DUMP, rib)
	if err != nil {
		panic(err)
	}
	nameToIndex := make(map[int]string)
	addrs, err := net.Interfaces()
	for _, addr := range addrs {
		nameToIndex[addr.Index] = addr.Name
	}

	m := make(map[string][]route.Addr)
	for _, msg := range msgs {
		message := msg.(*route.RouteMessage)
		if name, found := nameToIndex[message.Index]; found {
			if v, ok := m[name]; ok {
				temp := removeEmptyElement(message)
				m[name] = append(v, temp...)
			} else {
				m[name] = removeEmptyElement(message)
			}
		}
	}
	fmt.Println(m)
}

func removeEmptyElement(message *route.RouteMessage) []route.Addr {
	var temp []route.Addr
	for _, addr := range message.Addrs {
		if addr != nil {
			temp = append(temp, addr)
		}
	}
	return temp
}

func TestGetRouteTableByNetstat(t *testing.T) {
	ip := net.ParseIP("192.168.1.1")
	for i := range ip.To4() {
		fmt.Println(i)
	}
}
