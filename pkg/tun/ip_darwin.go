//go:build darwin || freebsd || openbsd

package tun

import (
	"context"
	"fmt"
	"net/netip"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func changeIP(ifName string, oldAddr, newAddr string) error {
	prefix, err := netip.ParsePrefix(newAddr)
	if err != nil {
		return fmt.Errorf("invalid address %s: %w", newAddr, err)
	}
	// macOS SIOCAIFADDR only ADDS an alias, so the old address must be explicitly
	// removed first — otherwise the device ends up with both IPs. Best-effort.
	if oldAddr != "" {
		if oldPrefix, perr := netip.ParsePrefix(oldAddr); perr == nil && oldPrefix.Addr() != prefix.Addr() {
			// Best-effort: the add below still proceeds, but log so a stale
			// address left on the device is diagnosable.
			if err = removeInterfaceAddress(ifName, oldPrefix.Addr()); err != nil {
				plog.G(context.Background()).Warnf("Failed to remove old TUN address %s from %s: %v", oldPrefix.Addr(), ifName, err)
			}
		}
	}
	return setInterfaceAddress(ifName, prefix)
}
