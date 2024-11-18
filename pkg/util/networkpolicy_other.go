//go:build !windows

package util

import (
	"context"
)

func DeleteBlockFirewallRule(ctx context.Context)    {}
func DeleteAllowFirewallRule(ctx context.Context)    {}
func FindAllowFirewallRule(ctx context.Context) bool { return false }
func AddAllowFirewallRule(ctx context.Context)       {}
