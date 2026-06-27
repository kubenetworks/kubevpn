package ssh

import (
	"strings"
	"testing"
)

func TestResolveProxyJumpChain_NoCycle(t *testing.T) {
	// Linear chain A -> B -> C with no cycle; expect 3 bastions.
	list := parseSSHConfig(t, `
Host A
  Hostname 10.0.0.1
  ProxyJump B

Host B
  Hostname 10.0.0.2
  ProxyJump C

Host C
  Hostname 10.0.0.3
`)
	bastions, err := resolveProxyJumpChain("A", SshConfig{}, list)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bastions) != 3 {
		t.Fatalf("expected 3 bastions, got %d", len(bastions))
	}
}

func TestResolveProxyJumpChain_Cycle(t *testing.T) {
	// A -> B -> A creates a cycle; expect an error containing "circular".
	list := parseSSHConfig(t, `
Host A
  Hostname 10.0.0.1
  ProxyJump B

Host B
  Hostname 10.0.0.2
  ProxyJump A
`)
	_, err := resolveProxyJumpChain("A", SshConfig{}, list)
	if err == nil {
		t.Fatal("expected error for circular ProxyJump, got nil")
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Fatalf("expected error containing 'circular', got: %v", err)
	}
}

func TestResolveProxyJumpChain_SingleNode(t *testing.T) {
	// No ProxyJump for the start alias; expect exactly 1 bastion.
	list := parseSSHConfig(t, `
Host A
  Hostname 10.0.0.1
`)
	bastions, err := resolveProxyJumpChain("A", SshConfig{}, list)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bastions) != 1 {
		t.Fatalf("expected 1 bastion, got %d", len(bastions))
	}
}
