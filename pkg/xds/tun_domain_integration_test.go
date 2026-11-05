package xds

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// TestIntegration_LeaseAdapterProposalAndRestart exercises the production RPC mapping,
// real lease/bitmap/store transactions and asynchronous Envoy adapter for two users.
func TestIntegration_LeaseAdapterProposalAndRestart(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	alice, err := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "alice", Hostname: "alice-host", ExcludeIPs: []string{"invalid", "198.18.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if parseV4(t, alice).Equal(net.ParseIP("198.18.0.1")) {
		t.Fatal("RPC exclusion was not parsed")
	}
	bob, err := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if alice.IPv4 == bob.IPv4 {
		t.Fatal("users share an address")
	}
	cmClient := env.server.clientset.CoreV1().ConfigMaps("test-ns")
	cm, err := cmClient.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var allocs map[string]map[string]any
	if err := yaml.Unmarshal([]byte(cm.Data[config.KeyTunAllocs]), &allocs); err != nil {
		t.Fatal(err)
	}
	if allocs["alice"]["hostname"] != "alice-host" {
		t.Fatal("hostname did not reach the store")
	}
	allocs["alice"]["ipv4"] = "198.18.200.200/16"
	// Round-tripping through map[string]any turns versions into float64 (53-bit
	// mantissa), corrupting UnixNano values; a real kubectl edit preserves the
	// int64 field. Restore the exact versions so the stale-echo guard in
	// proposeManualChange (want.Version < liveVersion) does not silently reject
	// the edit as stale.
	allocs["alice"]["version"] = alice.Version
	allocs["bob"]["version"] = bob.Version
	raw, err := yaml.Marshal(allocs)
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[config.KeyTunAllocs] = string(raw)
	cm.Data[config.KeyEnvoy] = `[{"namespace":"test-ns","uid":"deployments.apps.demo","schemaVersion":2,"rules":[{"ownerID":"alice","localTunIPv4":"` + parseV4(t, alice).String() + `"},{"ownerID":"bob","localTunIPv4":"` + parseV4(t, bob).String() + `"}]}]`
	if _, err := cmClient.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Propose while absent, then subscribe: pending intent must replay over actual gRPC.
	env.server.ReconcileAllocsFromConfigMap(ctx)
	stream, err := env.client.WatchTunIP(ctx, &rpc.TunIPRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if !proposal.DryRun || proposal.IPv4 != "198.18.200.200/32" || proposal.IPv6 != "" {
		t.Fatalf("proposal mapping: %+v", proposal)
	}
	confirmed, err := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "alice", ConfirmIP: "198.18.200.200"})
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.DryRun || confirmed.IPv4 != proposal.IPv4 || confirmed.IPv6 != alice.IPv6 || confirmed.Version <= alice.Version {
		t.Fatalf("confirm mapping: %+v", confirmed)
	}
	waitForEnvoyRuleIPv4(t, env, "alice", net.ParseIP("198.18.200.200"))
	waitForEnvoyRuleIPv4(t, env, "bob", parseV4(t, bob))
	bobRenew, err := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if bobRenew.IPv4 != bob.IPv4 || bobRenew.IPv6 != bob.IPv6 {
		t.Fatal("Alice changed Bob's allocation")
	}
	restarted, err := NewTunConfigServer(ctx, env.server.clientset, "test-ns")
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	// Same production RPC adapter after reload; no private state access.
	restored, err := restarted.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if restored.IPv4 != confirmed.IPv4 || restored.Version != confirmed.Version {
		t.Fatalf("restart lost committed state: %+v", restored)
	}
}
