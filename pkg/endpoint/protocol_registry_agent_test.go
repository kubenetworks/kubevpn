package endpoint

import (
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TestParseNodeAgentParam verifies the node-agent vpnScript listener URL parses
// the agent= query param alongside the existing route= param.
func TestParseNodeAgentParam(t *testing.T) {
	// This is exactly the URL rendered by vpnScript in pkg/inject/nodeagent_job.go
	// after bash expansion: ?route=${CIDR4}&agent=${POD_UID}
	node, err := ParseNode(`tun:/tcp://traffic-manager.kubevpn-ns:10801?route=198.18.0.0/16&agent=0f2a3b4c-pod-uid`)
	if err != nil {
		t.Fatalf("ParseNode: %v", err)
	}
	if node.Protocol != "tun" {
		t.Errorf("Protocol = %q, want %q", node.Protocol, "tun")
	}
	if node.Forward != "tcp://traffic-manager.kubevpn-ns:10801" {
		t.Errorf("Forward = %q, want %q", node.Forward, "tcp://traffic-manager.kubevpn-ns:10801")
	}
	if got := node.Get("route"); got != "198.18.0.0/16" {
		t.Errorf("route = %q, want %q", got, "198.18.0.0/16")
	}
	if got := node.Get("agent"); got != "0f2a3b4c-pod-uid" {
		t.Errorf("agent = %q, want %q", got, "0f2a3b4c-pod-uid")
	}
}

// TestParseNodeWithoutAgentParam verifies a listener URL without the agent
// param (mesh sidecar / user data plane) yields an empty identity override.
func TestParseNodeWithoutAgentParam(t *testing.T) {
	node, err := ParseNode(`tun:/tcp://traffic-manager.kubevpn-ns:10801?route=198.18.0.0/16`)
	if err != nil {
		t.Fatalf("ParseNode: %v", err)
	}
	if got := node.Get("agent"); got != "" {
		t.Errorf("agent = %q, want empty for non-agent tunnel", got)
	}
}

// TestResolveTunIPOwner verifies the agent= query param maps to the GetTunIP
// identity: non-empty → OwnerID=podUID IsAgent=true; empty → legacy sidecar
// identity (POD_NAME env, else "unknown") with IsAgent=false.
func TestResolveTunIPOwner(t *testing.T) {
	t.Run("agent param wins over env", func(t *testing.T) {
		t.Setenv(config.EnvPodName, "sidecar-pod")
		owner, isAgent := resolveTunIPOwner("agent-pod-uid")
		if owner != "agent-pod-uid" || !isAgent {
			t.Errorf("resolveTunIPOwner = (%q, %v), want (agent-pod-uid, true)", owner, isAgent)
		}
	})

	t.Run("no param falls back to POD_NAME env", func(t *testing.T) {
		t.Setenv(config.EnvPodName, "sidecar-pod")
		owner, isAgent := resolveTunIPOwner("")
		if owner != "sidecar-pod" || isAgent {
			t.Errorf("resolveTunIPOwner = (%q, %v), want (sidecar-pod, false)", owner, isAgent)
		}
	})

	t.Run("no param and no env yields unknown", func(t *testing.T) {
		t.Setenv(config.EnvPodName, "")
		owner, isAgent := resolveTunIPOwner("")
		if owner != "unknown" || isAgent {
			t.Errorf("resolveTunIPOwner = (%q, %v), want (unknown, false)", owner, isAgent)
		}
	})
}
