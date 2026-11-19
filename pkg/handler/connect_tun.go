package handler

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/wencaiwulue/kubevpn/v2/pkg/endpoint"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// Run serves the protocol listeners configured by the server command.
func Run(ctx context.Context, servers []endpoint.Server) error {
	if os.Getenv("KUBEVPN_AGENT_CLEANUP_IPTABLES") == "1" {
		// Set up signal handler for iptables cleanup
		cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
		defer cleanupCancel()
		stopChan := make(chan os.Signal, 1)
		signal.Notify(stopChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
		defer signal.Stop(stopChan)

		go func() {
			<-stopChan
			cleanupIptables(cleanupCtx)
			cleanupCancel()
		}()

		// Also clean up on normal exit
		defer cleanupIptables(context.Background())
	}

	return endpoint.Serve(ctx, servers)
}

// cleanupIptables removes the kubevpn-nodeagent rules from iptables and ip6tables
func cleanupIptables(ctx context.Context) {
	plog.G(ctx).Info("Cleaning up node-agent iptables rules")

	// Cleanup iptables (IPv4)
	if cmdPath, err := exec.LookPath("iptables"); err == nil {
		// List all PREROUTING rules and delete the ones with kubevpn-nodeagent comment
		cmd := exec.CommandContext(ctx, cmdPath, "-t", "nat", "-S", "PREROUTING")
		output, err := cmd.Output()
		if err == nil {
			// Split lines and delete matching rules in reverse order
			lines := splitLines(string(output))
			for i := len(lines) - 1; i >= 0; i-- {
				line := lines[i]
				if contains(line, "kubevpn-nodeagent") {
					// Remove the "-A " prefix and delete the rule
					if hasPrefix(line, "-A PREROUTING ") {
						ruleSpec := line[len("-A PREROUTING "):]
						args := []string{"-t", "nat", "-D", "PREROUTING"}
						args = append(args, splitSpaces(ruleSpec)...)
						_ = exec.CommandContext(ctx, cmdPath, args...).Run()
					}
				}
			}
		}
	}

	// Cleanup ip6tables (IPv6)
	if cmdPath, err := exec.LookPath("ip6tables"); err == nil {
		cmd := exec.CommandContext(ctx, cmdPath, "-t", "nat", "-S", "PREROUTING")
		output, err := cmd.Output()
		if err == nil {
			lines := splitLines(string(output))
			for i := len(lines) - 1; i >= 0; i-- {
				line := lines[i]
				if contains(line, "kubevpn-nodeagent") {
					if hasPrefix(line, "-A PREROUTING ") {
						ruleSpec := line[len("-A PREROUTING "):]
						args := []string{"-t", "nat", "-D", "PREROUTING"}
						args = append(args, splitSpaces(ruleSpec)...)
						_ = exec.CommandContext(ctx, cmdPath, args...).Run()
					}
				}
			}
		}
	}
}

// Simple string utilities to avoid importing heavy packages
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if start < i {
				lines = append(lines, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func contains(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(s) < len(substr) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func splitSpaces(s string) []string {
	var parts []string
	var buf []byte
	inSpace := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			if !inSpace {
				parts = append(parts, string(buf))
				buf = buf[:0]
				inSpace = true
			}
		} else {
			buf = append(buf, c)
			inSpace = false
		}
	}
	if !inSpace {
		parts = append(parts, string(buf))
	}
	return parts
}
