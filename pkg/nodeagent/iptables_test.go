package nodeagent

import (
	"context"
	"strings"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

type fakeRunner struct {
	listOutput string
	calls      []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return f.listOutput, nil
}

func (f *fakeRunner) commands() string { return strings.Join(f.calls, "\n") }

func TestConvergeHostDNAT_ConvergesAndExcludesAgent(t *testing.T) {
	runner := &fakeRunner{listOutput: "-A PREROUTING -p tcp -m set ! --match-set kubevpn-nodeagent src -d 10.244.0.5 -j DNAT --to 10.244.0.8:20001 -m comment --comment \"kubevpn-nodeagent\"\n"}
	attachments := []controlplane.NodeAttachment{{
		PodIP: "10.244.0.5", AgentListenerPort: 20000,
		Ports: []controlplane.AgentOriginPort{
			{ContainerPort: 9080, Protocol: "TCP", AgentListenerPort: 20001},
			{ContainerPort: 9081, Protocol: "UDP", AgentListenerPort: 20002},
		},
	}, {
		PodIP: "10.96.0.10", OriginIP: "10.96.0.10", ServiceName: "reviews", AgentListenerPort: 20000,
		Ports: []controlplane.AgentOriginPort{
			{ContainerPort: 9080, Protocol: "TCP", OriginPort: 80, AgentListenerPort: 20003},
		},
	}}
	if err := ConvergeHostDNAT(context.Background(), runner, "10.244.0.8", []string{"10.244.0.8", "10.244.0.9"}, attachments); err != nil {
		t.Fatal(err)
	}
	joined := runner.commands()
	if !strings.Contains(joined, "-D PREROUTING") {
		t.Fatal("old marked rule was not flushed")
	}
	if !strings.Contains(joined, "-m set ! --match-set kubevpn-nodeagent src") {
		t.Fatal("node-agent ipset exclusion is missing")
	}
	if !strings.Contains(joined, "-p udp -m set ! --match-set kubevpn-nodeagent src -d 10.244.0.5 --dport 9081 -j DNAT --to 10.244.0.8:20002") {
		t.Fatal("UDP DNAT rule with original-port mapping missing")
	}
	if !strings.HasPrefix(joined, "nsenter --net=/host/proc/1/ns/net") {
		t.Fatal("rule was not applied in host netns")
	}
	if !strings.Contains(joined, "-I PREROUTING 1 -p tcp -m set ! --match-set kubevpn-nodeagent src -d 10.244.0.5 --dport 9080 -j DNAT --to 10.244.0.8:20001") {
		t.Fatal("expected TCP original-port DNAT rule not installed")
	}
	if !strings.Contains(joined, "-I PREROUTING 1 -p tcp -m set ! --match-set kubevpn-nodeagent src -d 10.96.0.10 --dport 80 -j DNAT --to 10.244.0.8:20003") {
		t.Fatal("expected Service ClusterIP DNAT rule not installed")
	}
	if strings.Count(joined, "-I PREROUTING 1 -p") != 3 {
		t.Fatalf("expected pod TCP/UDP and service TCP add operations, got:\n%s", joined)
	}
	if !strings.Contains(joined, "ipset add kubevpn-nodeagent 10.244.0.9") {
		t.Fatalf("remote node-agent IP missing from exclusion set:\n%s", joined)
	}
	if strings.Contains(joined, "-j RETURN") {
		t.Fatalf("source RETURN must not bypass kube-proxy/IPVS:\n%s", joined)
	}
}

func TestConvergeHostDNAT_RejectsDuplicateDestination(t *testing.T) {
	runner := &fakeRunner{}
	attachments := []controlplane.NodeAttachment{
		{PodIP: "10.96.0.10", Ports: []controlplane.AgentOriginPort{{ContainerPort: 9080, OriginPort: 80, Protocol: "TCP", AgentListenerPort: 20001}}},
		{PodIP: "10.96.0.10", Ports: []controlplane.AgentOriginPort{{ContainerPort: 9080, OriginPort: 80, Protocol: "TCP", AgentListenerPort: 20002}}},
	}
	if err := ConvergeHostDNAT(context.Background(), runner, "10.244.0.8", nil, attachments); err == nil {
		t.Fatal("expected duplicate Service destination to be rejected")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("invalid attachments must be rejected before flushing working rules: %v", runner.calls)
	}
}

func TestConvergeHostDNAT_EmptyAttachmentsFlushesMarkedRules(t *testing.T) {
	runner := &fakeRunner{listOutput: "-A PREROUTING -p tcp -d 10.244.0.5 -j DNAT --to 10.244.0.8:20001 -m comment --comment \"kubevpn-nodeagent\"\n"}
	if err := ConvergeHostDNAT(context.Background(), runner, "10.244.0.8", nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(runner.commands(), "-I PREROUTING 1") {
		t.Fatal("must not add rules for empty attachments")
	}
	if !strings.Contains(runner.commands(), "-D PREROUTING") {
		t.Fatal("old marked rule was not flushed")
	}
}
