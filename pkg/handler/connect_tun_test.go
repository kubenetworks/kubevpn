package handler

import (
	"reflect"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestAssertAgentIptablesArgs(t *testing.T) {
	// Test IPv4 args construction
	testCIDR4 := "198.18.0.0/16"
	args4 := BuildIptablesAppendArgs("iptables", testCIDR4)
	expectedPrefix4 := []string{"-t", "nat", "-A", "PREROUTING"}
	// Verify prefix
	if !reflect.DeepEqual(args4[:4], expectedPrefix4) {
		t.Fatalf("iptables prefix mismatch:\ngot  %v\nwant %v", args4[:4], expectedPrefix4)
	}
	// Get rule spec tokens from config and substitute CIDR
	expectedSpecTokens4 := SplitRuleSpec(config.AgentDNATRuleSpec4)
	// Substitute the CIDR placeholder
	for i := range expectedSpecTokens4 {
		if expectedSpecTokens4[i] == "${CIDR4}" {
			expectedSpecTokens4[i] = testCIDR4
		}
	}
	// Verify the rest of the args match
	actualSpecTokens4 := args4[4:]
	if !reflect.DeepEqual(actualSpecTokens4, expectedSpecTokens4) {
		t.Fatalf("iptables rule spec mismatch:\ngot  %v\nwant %v", actualSpecTokens4, expectedSpecTokens4)
	}
	// Verify the comment token is present and correct
	foundComment4 := false
	for _, token := range actualSpecTokens4 {
		if token == config.AgentIptablesComment {
			foundComment4 = true
			break
		}
	}
	if !foundComment4 {
		t.Errorf("iptables missing comment token %q", config.AgentIptablesComment)
	}

	// Test IPv6 args construction
	testCIDR6 := "2001:2::/64"
	args6 := BuildIptablesAppendArgs("ip6tables", testCIDR6)
	expectedPrefix6 := []string{"-t", "nat", "-A", "PREROUTING"}
	// Verify prefix
	if !reflect.DeepEqual(args6[:4], expectedPrefix6) {
		t.Fatalf("ip6tables prefix mismatch:\ngot  %v\nwant %v", args6[:4], expectedPrefix6)
	}
	// Get rule spec tokens from config and substitute CIDR
	expectedSpecTokens6 := SplitRuleSpec(config.AgentDNATRuleSpec6)
	// Substitute the CIDR placeholder
	for i := range expectedSpecTokens6 {
		if expectedSpecTokens6[i] == "${CIDR6}" {
			expectedSpecTokens6[i] = testCIDR6
		}
	}
	// Verify the rest of the args match
	actualSpecTokens6 := args6[4:]
	if !reflect.DeepEqual(actualSpecTokens6, expectedSpecTokens6) {
		t.Fatalf("ip6tables rule spec mismatch:\ngot  %v\nwant %v", actualSpecTokens6, expectedSpecTokens6)
	}
	// Verify the comment token is present and correct
	foundComment6 := false
	for _, token := range actualSpecTokens6 {
		if token == config.AgentIptablesComment {
			foundComment6 = true
			break
		}
	}
	if !foundComment6 {
		t.Errorf("ip6tables missing comment token %q", config.AgentIptablesComment)
	}
}

func TestAgentIptablesConstants(t *testing.T) {
	// Just verify the comment constant is correct
	if config.AgentIptablesComment != "kubevpn-nodeagent" {
		t.Errorf("AgentIptablesComment = %q, want %q", config.AgentIptablesComment, "kubevpn-nodeagent")
	}
}
