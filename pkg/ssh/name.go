package ssh

import (
	"fmt"
	"net"
	"strings"
	"unicode"
)

func IPToFilename(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return "invalid-ip"
	}

	var filename string

	if ip.To4() != nil {
		filename = ip.String()
	} else {
		filename = convertIPv6(ip)
	}

	return sanitizeFilename(filename)
}

func convertIPv6(ip net.IP) string {
	ip = ip.To16()
	if ip == nil {
		return "invalid-ipv6"
	}

	var zone string
	if strings.Contains(ip.String(), "%") {
		parts := strings.Split(ip.String(), "%")
		if len(parts) > 1 {
			zone = parts[1]
		}
	}

	base := fmt.Sprintf("%02x%02x-%02x%02x-%02x%02x-%02x%02x",
		ip[0], ip[1], ip[2], ip[3],
		ip[4], ip[5], ip[6], ip[7],
	)

	if zone != "" {
		zone = sanitizeZone(zone)
		return base + "--" + zone
	}
	return base
}

func sanitizeZone(zone string) string {
	var result strings.Builder
	for _, r := range zone {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			result.WriteRune(r)
		} else {
			result.WriteRune('-')
		}
	}
	return result.String()
}

func sanitizeFilename(name string) string {
	var result strings.Builder
	lastWasDash := false

	for _, r := range name {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			result.WriteRune(r)
			lastWasDash = false

		case r == '-' || r == '_' || r == '.':
			if r == '.' {
				if !lastWasDash {
					result.WriteRune(r)
					lastWasDash = true
				}
			} else {
				result.WriteRune(r)
				lastWasDash = true
			}

		default:
			if !lastWasDash {
				result.WriteRune('-')
				lastWasDash = true
			}
		}
	}
	fname := result.String()
	fname = strings.Trim(fname, "-_.")
	if fname == "" {
		return "ip-address"
	}
	return fname
}
