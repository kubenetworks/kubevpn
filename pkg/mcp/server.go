// Package mcp implements the `kubevpn mcp` stdio MCP server. It is a thin adapter
// over the user daemon's gRPC API (pkg/daemon): tool handlers hold no business
// logic, sessions live daemon-side, and destructive operations (Uninstall, Reset,
// Upgrade, Quit) are deliberately not exposed.
package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pkgdaemon "github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
)

// Options carries everything the command layer resolves once at startup.
type Options struct {
	// KubeconfigBytes is the resolved kubeconfig (util.ConvertToKubeConfigBytes),
	// sent as ConnectRequest.KubeconfigBytes / ProxyRequest.KubeconfigBytes.
	KubeconfigBytes []byte
	// Namespace is the default namespace from the resolved kubeconfig.
	Namespace string
	// OriginKubeconfigPath is the resolved kubeconfig path
	// (util.GetKubeConfigPath), sent as OriginKubeconfigPath.
	OriginKubeconfigPath string
	// Version is the client version reported by the kubevpn_version tool.
	Version string
}

// Run serves MCP over stdio until ctx is cancelled or stdin closes.
func Run(ctx context.Context, opts Options) error {
	cli, err := pkgdaemon.GetClient(false)
	if err != nil {
		return err
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "kubevpn", Version: opts.Version}, nil)
	registerTools(server, opts, cli)
	return server.Run(ctx, &mcp.StdioTransport{})
}
