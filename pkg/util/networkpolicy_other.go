//go:build !windows

package util

import (
	"context"
)

func DeleteBlockFirewallRule(_ context.Context) {
}

func AddAllowFirewallRule() {
}

func DeleteAllowFirewallRule() {
}

func FindAllowFirewallRule() bool {
	return false
}
