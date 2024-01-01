package auto

import (
	"github.com/coredns/coredns/plugin/transfer"

	"github.com/miekg/dns"
)

// Transfer implements the transfer.Transfer interface.
func (a Auto) Transfer(zone string, serial uint32) (<-chan []dns.RR, error) {
	a.Zones.RLock()
	z, ok := a.Zones.Z[zone]
	a.Zones.RUnlock()

	if !ok || z == nil {
		return nil, transfer.ErrNotAuthoritative
	}
	return z.Transfer(serial)
}

// Notify sends notifies for all zones with secondaries configured with the transfer plugin
func (a Auto) Notify() error {
	var err error
	for _, origin := range a.Zones.Names() {
		e := a.transfer.Notify(origin)
		if e != nil {
			err = e
		}
	}
	return err
}
