package nodeagent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

// CommandRunner makes host-netns command execution testable without a cluster.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(output), err
}

// ConvergeHostDNAT flushes only kubevpn-marked rules, then installs exact
// podIP → agentIP:port rules for TCP and UDP. The agent-source exclusion is
// mandatory: otherwise the agent's origin-forwarding is captured again.
func ConvergeHostDNAT(ctx context.Context, runner CommandRunner, agentIP string, attachments []controlplane.NodeAttachment) error {
	if runner == nil || agentIP == "" {
		return fmt.Errorf("runner and agent IP are required")
	}
	const (
		prefix  = "nsenter"
		hostNet = "--net=/host/proc/1/ns/net"
	)
	list, err := runner.Run(ctx, prefix, hostNet, "iptables", "-t", "nat", "-S", "PREROUTING")
	if err != nil {
		return fmt.Errorf("list host PREROUTING rules: %w", err)
	}
	for _, line := range strings.Split(list, "\n") {
		if !strings.Contains(line, config.AgentIptablesComment) {
			continue
		}
		spec := strings.TrimSpace(strings.TrimPrefix(line, "-A "))
		if spec == "" {
			continue
		}
		args := []string{hostNet, "iptables", "-t", "nat", "-D"}
		args = append(args, strings.Fields(spec)...)
		if _, err := runner.Run(ctx, prefix, args...); err != nil {
			return fmt.Errorf("delete marked host rule: %w", err)
		}
	}
	for _, attachment := range attachments {
		for _, port := range attachment.Ports {
			protocol := strings.ToLower(string(port.Protocol))
			if protocol != "tcp" && protocol != "udp" {
				continue
			}
			args := []string{hostNet, "iptables", "-t", "nat", "-A", "PREROUTING",
				"-p", protocol, "!", "-s", agentIP, "-d", attachment.PodIP,
				"--dport", fmt.Sprintf("%d", port.DestinationPort()),
				"-j", "DNAT", "--to", fmt.Sprintf("%s:%d", agentIP, port.AgentListenerPort),
				"-m", "comment", "--comment", config.AgentIptablesComment,
			}
			if _, err := runner.Run(ctx, prefix, args...); err != nil {
				return fmt.Errorf("install host DNAT for pod %s: %w", attachment.PodIP, err)
			}
		}
	}
	return nil
}
