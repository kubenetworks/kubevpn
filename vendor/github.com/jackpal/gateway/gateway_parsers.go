package gateway

// References
// * https://superuser.com/questions/622144/what-does-netstat-r-on-osx-tell-you-about-gateways
// * https://man.freebsd.org/cgi/man.cgi?query=netstat&sektion=1

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	ns_destination = "Destination"
	ns_flags       = "Flags"
	ns_netif       = "Netif"
	ns_gateway     = "Gateway"
	ns_interface   = "Interface"
)

type netstatFields map[string]int

type windowsRouteStruct struct {
	// Dotted IP address
	Gateway string

	// Dotted IP address
	Interface string
}

type linuxRouteStruct struct {
	// Name of interface
	Iface string

	// big-endian hex string
	Gateway string
}

type unixRouteStruct struct {
	// Name of interface
	Iface string

	// Dotted IP address
	Gateway string
}

func fieldNum(name string, fields []string) int {
	// Return the zero-based index of given field in slice of field names
	for num, field := range fields {
		if name == field {
			return num
		}
	}

	return -1
}

func discoverFields(output []byte) (int, netstatFields) {
	// Discover positions of fields of interest in netstat output
	nf := make(netstatFields, 4)

	outputLines := strings.Split(string(output), "\n")
	for lineNo, line := range outputLines {
		fields := strings.Fields(line)

		if len(fields) > 3 {
			d, f, g, netif, iface := fieldNum(ns_destination, fields), fieldNum(ns_flags, fields), fieldNum(ns_gateway, fields), fieldNum(ns_netif, fields), fieldNum(ns_interface, fields)
			if d >= 0 && f >= 0 && g >= 0 && (netif >= 0 || iface >= 0) {
				nf[ns_destination] = d
				nf[ns_flags] = f
				nf[ns_gateway] = g
				if iface > 0 {
					// NetBSD
					nf[ns_netif] = iface
				} else {
					// Other BSD/Solaris/Darwin
					nf[ns_netif] = netif
				}

				return lineNo, nf
			}
		}
	}

	// Unable to parse column headers
	return -1, nil
}

func flagsContain(flags string, flag ...string) bool {
	// Check route table flags field for existence of specific flags
	contain := true

	for _, f := range flag {
		contain = contain && strings.Contains(flags, f)
	}

	return contain
}

func parseToWindowsRouteStruct(output []byte) (windowsRouteStruct, error) {
	// Windows route output format is always like this:
	// ===========================================================================
	// Interface List
	// 8 ...00 12 3f a7 17 ba ...... Intel(R) PRO/100 VE Network Connection
	// 1 ........................... Software Loopback Interface 1
	// ===========================================================================
	// IPv4 Route Table
	// ===========================================================================
	// Active Routes:
	// Network Destination        Netmask          Gateway       Interface  Metric
	//           0.0.0.0          0.0.0.0      192.168.1.1    192.168.1.100     20
	// ===========================================================================
	//
	// Windows commands are localized, so we can't just look for "Active Routes:" string
	// I'm trying to pick the active route,
	// then jump 2 lines and get the row
	// Not using regex because output is quite standard from Windows XP to 8 (NEEDS TESTING)
	//
	// If multiple default gateways are present, then the one with the lowest metric is returned.
	type gatewayEntry struct {
		gateway string
		iface   string
		metric  int
	}

	ipRegex := regexp.MustCompile(`^(((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)(\.|$)){4})`)
	defaultRoutes := make([]gatewayEntry, 0, 2)
	lines := strings.Split(string(output), "\n")
	sep := 0
	for idx, line := range lines {
		if sep == 3 {
			// We just entered the 2nd section containing "Active Routes:"
			if len(lines) <= idx+2 {
				return windowsRouteStruct{}, &ErrNoGateway{}
			}

			inputLine := lines[idx+2]
			if strings.HasPrefix(inputLine, "=======") {
				// End of routes
				break
			}
			fields := strings.Fields(inputLine)
			if len(fields) < 5 || !ipRegex.MatchString(fields[0]) {
				return windowsRouteStruct{}, &ErrCantParse{}
			}

			if fields[0] != "0.0.0.0" {
				// Routes to 0.0.0.0 are listed first
				// so we are done
				break
			}

			metric, err := strconv.Atoi(fields[4])

			if err != nil {
				return windowsRouteStruct{}, err
			}

			defaultRoutes = append(defaultRoutes, gatewayEntry{
				gateway: fields[2],
				iface:   fields[3],
				metric:  metric,
			})
		}
		if strings.HasPrefix(line, "=======") {
			sep++
			continue
		}
	}

	if sep == 0 {
		// We saw no separator lines, so input must have been garbage.
		return windowsRouteStruct{}, &ErrCantParse{}
	}

	if len(defaultRoutes) == 0 {
		return windowsRouteStruct{}, &ErrNoGateway{}
	}

	minDefaultRoute := slices.MinFunc(defaultRoutes,
		func(a, b gatewayEntry) int {
			return a.metric - b.metric
		})

	return windowsRouteStruct{
		Gateway:   minDefaultRoute.gateway,
		Interface: minDefaultRoute.iface,
	}, nil
}

func parseToLinuxRouteStruct(output []byte) (linuxRouteStruct, error) {
	// parseLinuxProcNetRoute parses the route file located at /proc/net/route
	// and returns the IP address of the default gateway. The default gateway
	// is the one with Destination value of 0.0.0.0.
	//
	// The Linux route file has the following format:
	//
	// $ cat /proc/net/route
	//
	// Iface   Destination Gateway     Flags   RefCnt  Use Metric  Mask
	// eno1    00000000    C900A8C0    0003    0   0   100 00000000    0   00
	// eno1    0000A8C0    00000000    0001    0   0   100 00FFFFFF    0   00
	const (
		sep              = "\t" // field separator
		destinationField = 1    // field containing hex destination address
		gatewayField     = 2    // field containing hex gateway address
		maskField        = 7    // field containing hex mask
	)
	scanner := bufio.NewScanner(bytes.NewReader(output))

	// Skip header line
	if !scanner.Scan() {
		err := scanner.Err()
		if err == nil {
			return linuxRouteStruct{}, &ErrNoGateway{}
		}

		return linuxRouteStruct{}, err
	}

	for scanner.Scan() {
		row := scanner.Text()
		tokens := strings.Split(row, sep)
		if len(tokens) < 11 {
			return linuxRouteStruct{}, &ErrInvalidRouteFileFormat{row: row}
		}

		// The default interface is the one that's 0 for both destination and mask.
		if !(tokens[destinationField] == "00000000" && tokens[maskField] == "00000000") {
			continue
		}

		return linuxRouteStruct{
			Iface:   tokens[0],
			Gateway: tokens[2],
		}, nil
	}
	return linuxRouteStruct{}, &ErrNoGateway{}
}

func parseWindowsGatewayIP(output []byte) (net.IP, error) {
	parsedOutput, err := parseToWindowsRouteStruct(output)
	if err != nil {
		return nil, err
	}

	ip := net.ParseIP(parsedOutput.Gateway)
	if ip == nil {
		return nil, &ErrCantParse{}
	}
	return ip, nil
}

func parseWindowsInterfaceIP(output []byte) (net.IP, error) {
	parsedOutput, err := parseToWindowsRouteStruct(output)
	if err != nil {
		return nil, err
	}

	ip := net.ParseIP(parsedOutput.Interface)
	if ip == nil {
		return nil, &ErrCantParse{}
	}
	return ip, nil
}

func parseLinuxGatewayIP(output []byte) (net.IP, error) {
	parsedStruct, err := parseToLinuxRouteStruct(output)
	if err != nil {
		return nil, err
	}

	// cast hex address to uint32
	d, err := strconv.ParseUint(parsedStruct.Gateway, 16, 32)
	if err != nil {
		return nil, fmt.Errorf(
			"parsing default interface address field hex %q: %w",
			parsedStruct.Gateway,
			err,
		)
	}
	// make net.IP address from uint32
	ipd32 := make(net.IP, 4)
	binary.LittleEndian.PutUint32(ipd32, uint32(d))
	return ipd32, nil
}

func parseLinuxInterfaceIP(output []byte) (net.IP, error) {
	// Return the first IPv4 address we encounter.
	return parseLinuxInterfaceIPImpl(output, &intefaceGetterImpl{})
}

func parseLinuxInterfaceIPImpl(output []byte, ifaceGetter interfaceGetter) (net.IP, error) {
	// Mockable implemenation
	parsedStruct, err := parseToLinuxRouteStruct(output)
	if err != nil {
		return nil, err
	}

	return getInterfaceIP4(parsedStruct.Iface, ifaceGetter)
}

func parseUnixInterfaceIP(output []byte) (net.IP, error) {
	// Return the first IPv4 address we encounter.
	return parseUnixInterfaceIPImpl(output, &intefaceGetterImpl{})
}

func parseUnixInterfaceIPImpl(output []byte, ifaceGetter interfaceGetter) (net.IP, error) {
	// Mockable implemenation
	parsedStruct, err := parseNetstatToRouteStruct(output)
	if err != nil {
		return nil, err
	}

	return getInterfaceIP4(parsedStruct.Iface, ifaceGetter)
}

func getInterfaceIP4(name string, ifaceGetter interfaceGetter) (net.IP, error) {
	// Given interface name and an interface to "net" package
	// lookup ip4 for the given interface
	iface, err := ifaceGetter.InterfaceByName(name)
	if err != nil {
		return nil, err
	}

	addrs, err := ifaceGetter.Addrs(iface)
	if err != nil {
		return nil, err
	}

	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}

		ip := ipnet.IP.To4()
		if ip != nil {
			return ip, nil
		}
	}

	return nil, fmt.Errorf("no IPv4 address found for interface %v",
		name)
}

func parseUnixGatewayIP(output []byte) (net.IP, error) {
	// Extract default gateway IP from netstat route table
	parsedStruct, err := parseNetstatToRouteStruct(output)
	if err != nil {
		return nil, err
	}

	ip := net.ParseIP(parsedStruct.Gateway)
	if ip == nil {
		return nil, &ErrCantParse{}
	}

	return ip, nil
}

// Parse any netstat -rn output
func parseNetstatToRouteStruct(output []byte) (unixRouteStruct, error) {
	startLine, nsFields := discoverFields(output)

	if startLine == -1 {
		// Unable to find required column headers in netstat output
		return unixRouteStruct{}, &ErrCantParse{}
	}

	outputLines := strings.Split(string(output), "\n")

	for lineNo, line := range outputLines {
		if lineNo <= startLine || strings.Contains(line, "-----") {
			// Skip until past column headers and heading underlines (solaris)
			continue
		}

		fields := strings.Fields(line)

		if len(fields) < 4 {
			// past route entries (got to end or blank line prior to ip6 entries)
			break
		}

		if fields[nsFields[ns_destination]] == "default" && flagsContain(fields[nsFields[ns_flags]], "U", "G") {
			iface := ""
			if ifaceIdx := nsFields[ns_netif]; ifaceIdx < len(fields) {
				iface = fields[nsFields[ns_netif]]
			}
			return unixRouteStruct{
				Iface:   iface,
				Gateway: fields[nsFields[ns_gateway]],
			}, nil
		}
	}

	return unixRouteStruct{}, &ErrNoGateway{}
}
