package main

import (
	"bufio"
	"kubevpn/core"
	"net"
	"os"
	"strings"
)

func parseIP(s string, port string) (ips []string) {
	if s == "" {
		return
	}
	if port == "" {
		port = "8080" // default port
	}

	file, err := os.Open(s)
	if err != nil {
		ss := strings.Split(s, ",")
		for _, s := range ss {
			s = strings.TrimSpace(s)
			if s != "" {
				// TODO: support IPv6
				if !strings.Contains(s, ":") {
					s = s + ":" + port
				}
				ips = append(ips, s)
			}

		}
		return
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, ":") {
			line = line + ":" + port
		}
		ips = append(ips, line)
	}
	return
}

func parseIPRoutes(routeStringList string) (routes []core.IPRoute) {
	if len(routeStringList) == 0 {
		return
	}

	ss := strings.Split(routeStringList, ",")
	for _, s := range ss {
		if _, inet, _ := net.ParseCIDR(strings.TrimSpace(s)); inet != nil {
			routes = append(routes, core.IPRoute{Dest: inet})
		}
	}
	return
}
