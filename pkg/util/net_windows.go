//go:build windows

package util

import (
	"github.com/pkg/errors"
	"golang.org/x/sys/windows/registry"
)

/*
*
Reference: https://learn.microsoft.com/en-us/troubleshoot/windows-server/networking/configure-ipv6-in-windows#use-registry-key-to-configure-ipv6

| IPv6 Functionality                                 | Registry value and comments                                                                   |
|----------------------------------------------------|-----------------------------------------------------------------------------------------------|
| Prefer IPv4 over IPv6                              | Decimal 32<br>Hexadecimal 0x20<br>Binary xx1x xxxx<br>Recommended instead of disabling IPv6.  |
| Disable IPv6                                       | Decimal 255<br>Hexadecimal 0xFF<br>Binary 1111 1111<br>										 |
| Disable IPv6 on all nontunnel interfaces           | Decimal 16<br>Hexadecimal 0x10<br>Binary xxx1 xxxx                                            |
| Disable IPv6 on all tunnel interfaces              | Decimal 1<br>Hexadecimal 0x01<br>Binary xxxx xxx1                                             |
| Disable IPv6 on all nontunnel interfaces (except the loopback) and on IPv6 tunnel interface | Decimal 17<br>Hexadecimal 0x11<br>Binary xxx1 xxx1   |
| Prefer IPv6 over IPv4                              | Binary xx0x xxxx                                                                              |
| Re-enable IPv6 on all nontunnel interfaces         | Binary xxx0 xxxx                                                                              |
| Re-enable IPv6 on all tunnel interfaces            | Binary xxx xxx0                                                                               |
| Re-enable IPv6 on nontunnel interfaces and on IPv6 tunnel interfaces | Binary xxx0 xxx0                                                            |

Enable IPv6:

	  Default Value 		Hexadecimal 0x00
							Decimal 0
	  Prefer IPv4 over IPv6	Hexadecimal 0x20
	  						Decimal 32
	  Prefer IPv6 over IPv4	Binary xx0x xxxx
*/
func IsIPv6Enabled() (bool, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\Tcpip6\Parameters`, registry.QUERY_VALUE)
	if err != nil {
		return false, err
	}
	defer key.Close()

	val, valtype, err := key.GetIntegerValue("DisabledComponents")
	if errors.Is(err, registry.ErrNotExist) {
		return true, nil
	}

	if err != nil {
		return false, err
	}

	if valtype != registry.DWORD {
		return false, nil
	}

	switch val {
	case 0x00:
		return true, nil
	case 0x20:
		return true, nil
	default:
		return false, nil
	}
}
