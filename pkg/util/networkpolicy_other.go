//go:build !windows

package util

import (
	"context"
)

func DeleteBlockFirewallRule(ctx context.Context) {}
