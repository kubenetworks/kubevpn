package controlplane

import (
	"context"
	"testing"

	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

func TestNotifyMessage(t *testing.T) {
	msg := NotifyMessage{Content: "test-envoy-config"}
	if msg.Content != "test-envoy-config" {
		t.Fatalf("expected Content %q, got %q", "test-envoy-config", msg.Content)
	}

	empty := NotifyMessage{}
	if empty.Content != "" {
		t.Fatalf("expected empty Content, got %q", empty.Content)
	}
}

// TestWatchSignature is a compile-time verification that Watch accepts
// (context.Context, cmdutil.Factory, chan<- NotifyMessage) and returns error.
func TestWatchSignature(t *testing.T) {
	// Assign Watch to a typed variable to confirm the signature at compile time.
	var fn func(context.Context, cmdutil.Factory, chan<- NotifyMessage, ...OnDHCPChange) error
	fn = Watch
	if fn == nil {
		t.Fatal("Watch function is nil")
	}
}
