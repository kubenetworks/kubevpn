package nodeagent

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
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

// ConvergeHostDNAT flushes only kubevpn-marked rules, then inserts exact
// destination IP/port → agentIP:port rules at the head of PREROUTING, before
// kube-proxy can consume Service traffic. The agent-source exclusion is
// mandatory: otherwise the agent's origin-forwarding is captured again.
func ConvergeHostDNAT(ctx context.Context, runner CommandRunner, agentIP string, agentSourceIPs []string, attachments []controlplane.NodeAttachment) error {
	if runner == nil || agentIP == "" {
		return fmt.Errorf("runner and agent IP are required")
	}
	if err := validateAttachmentDestinations(attachments); err != nil {
		return err
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
			args := []string{hostNet, "iptables", "-t", "nat", "-I", "PREROUTING", "1",
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
	if len(attachments) != 0 {
		sources := map[string]struct{}{agentIP: {}}
		for _, sourceIP := range agentSourceIPs {
			if sourceIP != "" {
				sources[sourceIP] = struct{}{}
			}
		}
		ordered := make([]string, 0, len(sources))
		for sourceIP := range sources {
			ordered = append(ordered, sourceIP)
		}
		sort.Strings(ordered)
		for _, sourceIP := range ordered {
			args := []string{hostNet, "iptables", "-t", "nat", "-I", "PREROUTING", "1",
				"-s", sourceIP, "-j", "RETURN", "-m", "comment", "--comment", config.AgentIptablesComment,
			}
			if _, err := runner.Run(ctx, prefix, args...); err != nil {
				return fmt.Errorf("install node-agent source exclusion for %s: %w", sourceIP, err)
			}
		}
	}
	return nil
}

func validateAttachmentDestinations(attachments []controlplane.NodeAttachment) error {
	seen := make(map[string]struct{})
	for _, attachment := range attachments {
		for _, port := range attachment.Ports {
			protocol := strings.ToLower(string(port.Protocol))
			if protocol != "tcp" && protocol != "udp" {
				continue
			}
			key := fmt.Sprintf("%s/%s/%d", attachment.PodIP, protocol, port.DestinationPort())
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate node-agent destination %s", key)
			}
			seen[key] = struct{}{}
		}
	}
	return nil
}
