package main

import (
	"fmt"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func main() {
	fmt.Printf("IPv4Pool: %v\n", config.IPv4Pool)
	fmt.Printf("CIDR: %v\n", config.CIDR)

	fmt.Printf("\nIPv4UserPool: %v\n", config.IPv4UserPool)
	fmt.Printf("CIDRUser: %v\n", config.CIDRUser)

	fmt.Printf("\nIPv4AgentPool: %v\n", config.IPv4AgentPool)
	fmt.Printf("CIDRAgent: %v\n", config.CIDRAgent)

	// Let's see what the first IP in agent range is
	agentStart := config.CIDRAgent.IP.Mask(config.CIDRAgent.Mask)
	fmt.Printf("\nAgent range first IP: %v\n", agentStart)

	// Test incrementing
	ip := make(net.IP, len(agentStart))
	copy(ip, agentStart)
	fmt.Printf("ip after copy: %v\n", ip)
	for i := 0; i < 10; i++ {
		fmt.Printf("  ip: %v\n", ip)
		// Increment
		for j := len(ip) - 1; j >= 0; j-- {
			ip[j]++
			if ip[j] != 0 {
				break
			}
		}
	}
}
