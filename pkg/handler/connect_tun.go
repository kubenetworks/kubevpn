package handler

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/endpoint"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// Run serves the protocol listeners configured by the server command.
func Run(ctx context.Context, servers []endpoint.Server) error {
	cleanupEnv := os.Getenv("KUBEVPN_AGENT_CLEANUP_IPTABLES") == "1"
	maintainEnv := os.Getenv("KUBEVPN_AGENT_MAINTAIN_IPTABLES") == "1"

	if !cleanupEnv && !maintainEnv {
		return endpoint.Serve(ctx, servers)
	}

	// Set up signal handler for iptables cleanup: flush the node-agent DNAT
	// rules, then cancel the serve context so the process exits promptly
	// (the pod's grace period must cover the flush, not a hung serve loop).
	serveCtx, serveCancel := context.WithCancel(ctx)
	defer serveCancel()
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	defer cleanupCancel()
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer signal.Stop(stopChan)

	go func() {
		<-stopChan
		flushAgentIptables(cleanupCtx)
		cleanupCancel()
		serveCancel()
	}()

	// Also clean up on normal exit
	defer flushAgentIptables(context.Background())

	// Start periodic iptables re-assertion if requested
	if maintainEnv {
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					assertAgentIptables(serveCtx)
				case <-serveCtx.Done():
					return
				}
			}
		}()
		// Initial assertion on startup
		assertAgentIptables(serveCtx)
	}

	return endpoint.Serve(serveCtx, servers)
}

// flushAgentIptables removes the kubevpn-nodeagent rules from iptables and ip6tables
func flushAgentIptables(ctx context.Context) {
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
				if contains(line, config.AgentIptablesComment) {
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
				if contains(line, config.AgentIptablesComment) {
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

// applyAgentIptables adds the kubevpn-nodeagent DNAT rules to iptables and ip6tables
func applyAgentIptables(ctx context.Context) {
	plog.G(ctx).Info("Applying node-agent iptables rules")

	// Apply iptables (IPv4)
	if cmdPath, err := exec.LookPath("iptables"); err == nil {
		var cidr string
		if config.CIDR != nil {
			cidr = config.CIDR.String()
		}
		if cidr == "" {
			cidr = os.Getenv("CIDR4")
		}
		if cidr != "" {
			args := BuildIptablesAppendArgs(cmdPath, cidr)
			_ = exec.CommandContext(ctx, cmdPath, args...).Run()
		}
	}

	// Apply ip6tables (IPv6)
	if cmdPath, err := exec.LookPath("ip6tables"); err == nil {
		var cidr string
		if config.CIDR6 != nil {
			cidr = config.CIDR6.String()
		}
		if cidr == "" {
			cidr = os.Getenv("CIDR6")
		}
		if cidr != "" {
			args := BuildIptablesAppendArgs(cmdPath, cidr)
			_ = exec.CommandContext(ctx, cmdPath, args...).Run()
		}
	}
}

// assertAgentIptables flushes and re-applies the kubevpn-nodeagent rules
func assertAgentIptables(ctx context.Context) {
	flushAgentIptables(ctx)
	applyAgentIptables(ctx)
}

// BuildIptablesAppendArgs builds the args for appending the node-agent DNAT rule
func BuildIptablesAppendArgs(cmdPath, cidr string) []string {
	var spec string
	if cmdPath == "ip6tables" {
		spec = config.AgentDNATRuleSpec6
	} else {
		spec = config.AgentDNATRuleSpec4
	}
	args := []string{"-t", "nat", "-A", "PREROUTING"}
	// Split the spec and substitute the CIDR placeholder
	tokens := SplitRuleSpec(spec)
	for _, token := range tokens {
		if token == "${CIDR4}" || token == "${CIDR6}" {
			args = append(args, cidr)
		} else {
			args = append(args, token)
		}
	}
	return args
}

// SplitRuleSpec splits a rule spec string into tokens, respecting quoted strings
// and treating "!" as a separate token.
func SplitRuleSpec(s string) []string {
	var tokens []string
	var buf []byte
	inQuote := false
	var quoteChar byte

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inQuote {
				buf = append(buf, c)
			} else if len(buf) > 0 {
				tokens = append(tokens, string(buf))
				buf = buf[:0]
			}
		case c == '"' || c == '\'':
			if inQuote && c == quoteChar {
				inQuote = false
				buf = append(buf, c)
				tokens = append(tokens, string(buf))
				buf = buf[:0]
			} else if !inQuote {
				inQuote = true
				quoteChar = c
				buf = append(buf, c)
			} else {
				buf = append(buf, c)
			}
		case c == '!' && !inQuote && len(buf) == 0:
			// Treat "!" as a separate token only when it's standalone at a token boundary
			tokens = append(tokens, "!")
		default:
			buf = append(buf, c)
		}
	}
	if len(buf) > 0 {
		tokens = append(tokens, string(buf))
	}
	return tokens
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
