package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// ConnectInput starts a VPN session. Sessions live daemon-side; the stream
// closes (EOF) once established.
type ConnectInput struct {
	Namespace      string `json:"namespace,omitempty" jsonschema:"target namespace; empty = the default namespace of the resolved kubeconfig"`
	KubeconfigPath string `json:"kubeconfigPath,omitempty" jsonschema:"path to a kubeconfig file; empty = the path resolved at server start"`
}

// ConnectOutput reports the established session.
type ConnectOutput struct {
	ConnectionID string   `json:"connectionId" jsonschema:"ID of the established connection; pass to kubevpn_disconnect"`
	Namespace    string   `json:"namespace"`
	Messages     []string `json:"messages" jsonschema:"last progress messages from the daemon"`
}

// connectTool is the plain function behind the kubevpn_connect tool.
func connectTool(ctx context.Context, d daemonAPI, opts Options, in ConnectInput) (*mcp.CallToolResult, *ConnectOutput, error) {
	kubeconfigBytes := opts.KubeconfigBytes
	if in.KubeconfigPath != "" {
		b, err := os.ReadFile(in.KubeconfigPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read kubeconfig: %w", err)
		}
		kubeconfigBytes = b
	}
	ns := in.Namespace
	if ns == "" {
		ns = opts.Namespace
	}
	stream, err := d.Connect(ctx)
	if err != nil {
		return nil, nil, wrapHint(err)
	}
	req := &rpc.ConnectRequest{
		KubeconfigBytes:      string(kubeconfigBytes),
		Namespace:            ns,
		OriginKubeconfigPath: opts.OriginKubeconfigPath,
		// Level 0 keeps daemon-side log verbosity at defaults; the agent reads
		// progress via the stream and kubevpn_logs.
		Level: 0,
	}
	if err := stream.Send(req); err != nil {
		return nil, nil, wrapHint(fmt.Errorf("send connect request: %w", err))
	}
	// The daemon sends the connection ID in a response frame, so use the typed
	// drain (not the message-only drainProgress).
	connID, msgs, err := drainConnect(stream)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, wrapHint(err)
	}
	out := &ConnectOutput{ConnectionID: connID, Namespace: ns, Messages: tail(msgs, 5)}
	return nil, out, nil
}

// drainConnect consumes the Connect stream to its terminal state, returning
// the last non-empty ConnectionID and all progress messages; io.EOF on success.
func drainConnect(s connectStream) (string, []string, error) {
	var connID string
	var msgs []string
	for {
		resp, err := s.Recv()
		if err != nil {
			return connID, msgs, err
		}
		if resp.GetConnectionID() != "" {
			connID = resp.GetConnectionID()
		}
		if resp.GetMessage() != "" {
			msgs = append(msgs, resp.GetMessage())
		}
	}
}
