package ssh

import (
	"testing"
)

func TestName(t *testing.T) {
	testIPs := []string{
		"192.168.1.1",
		"2001:0db8:85a3:0000:0000:8a2e:0370:7334",
		"::1",
		"fe80::1%eth0",
		"203.0.113.0",
		"invalid-ip",
		"::ffff:192.168.1.1",
	}

	for _, ip := range testIPs {
		t.Logf("%-35s => %s", ip, IPToFilename(ip))
	}
}
