package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTools wires every MCP tool to its handler. Uninstall/Reset/Upgrade/Quit
// are deliberately absent (docs/56 §4): destructive operations stay human-only.
func registerTools(server *mcp.Server, opts Options, d daemonAPI) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "kubevpn_status",
		Description: "List KubeVPN connections and their state (namespace, cluster, TUN IP). Check before connecting: reuse an existing Connected session.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in StatusInput) (*mcp.CallToolResult, *StatusOutput, error) {
		return statusTool(ctx, d, in)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "kubevpn_logs",
		Description: "Read the trailing N lines of KubeVPN daemon logs for a connection — the debugging channel when connect/proxy fails.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LogsInput) (*mcp.CallToolResult, *LogsOutput, error) {
		return logsTool(ctx, d, in)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "kubevpn_version",
		Description: "Report KubeVPN client and daemon versions.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in VersionInput) (*mcp.CallToolResult, *VersionOutput, error) {
		return versionTool(ctx, d, opts, in)
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "kubevpn_connect",
		Description: "Connect the local machine to a Kubernetes cluster network (VPN). Returns once established; the daemon owns the session. Scope proxies to this session afterwards.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ConnectInput) (*mcp.CallToolResult, *ConnectOutput, error) {
		return connectTool(ctx, d, opts, in)
	})
}
