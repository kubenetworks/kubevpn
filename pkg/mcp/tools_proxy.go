package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// ProxyInput intercepts workloads. Header scoping is mandatory from an agent:
// a bare proxy routes ALL traffic on declared ports to this machine.
type ProxyInput struct {
	Namespace string            `json:"namespace,omitempty" jsonschema:"workload namespace; empty = server default"`
	Workloads []string          `json:"workloads" jsonschema:"workloads to proxy, e.g. deploy/web or svc/orders"`
	Headers   map[string]string `json:"headers,omitempty" jsonschema:"REQUIRED from agents — traffic filter headers, e.g. {\"X-KubeVPN-User\":\"agent1\"}"`
	PortMap   []string          `json:"portMap,omitempty" jsonschema:"optional port mappings, e.g. 8888:8080"`
}

// ProxyOutput reports the result of the proxy operation.
type ProxyOutput struct {
	Messages []string `json:"messages" jsonschema:"progress messages from the daemon"`
}

// proxyTool is the plain function behind the kubevpn_proxy tool.
func proxyTool(ctx context.Context, d daemonAPI, opts Options, in ProxyInput) (*mcp.CallToolResult, *ProxyOutput, error) {
	if len(in.Workloads) == 0 {
		return nil, nil, fmt.Errorf("at least one workload is required")
	}
	if len(in.Headers) == 0 {
		return nil, nil, fmt.Errorf("agents must scope proxy with headers (docs/agents-guide.md): pass headers such as {\"X-KubeVPN-User\": \"<agent-id>\"} to avoid routing other users' traffic to this machine")
	}
	stream, err := d.Proxy(ctx)
	if err != nil {
		return nil, nil, wrapHint(err)
	}
	req := &rpc.ProxyRequest{
		KubeconfigBytes:      string(opts.KubeconfigBytes),
		Namespace:            in.Namespace,
		Headers:              in.Headers,
		PortMap:              in.PortMap,
		Workloads:            in.Workloads,
		OriginKubeconfigPath: opts.OriginKubeconfigPath,
		Level:                0,
	}
	if err := stream.Send(req); err != nil {
		return nil, nil, wrapHint(fmt.Errorf("send proxy request: %w", err))
	}
	_, msgs, err := drainProgress[rpc.ProxyResponse](stream)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, wrapHint(err)
	}
	return nil, &ProxyOutput{Messages: tail(msgs, 5)}, nil
}

// LeaveInput restores workloads to their original spec (undo proxy).
type LeaveInput struct {
	Namespace string   `json:"namespace,omitempty"`
	Workloads []string `json:"workloads,omitempty" jsonschema:"workloads to leave; empty = all resources owned by this session"`
}

// LeaveOutput reports the leave result.
type LeaveOutput struct {
	Messages []string `json:"messages"`
}

// leaveTool is the plain function behind the kubevpn_leave tool.
func leaveTool(ctx context.Context, d daemonAPI, opts Options, in LeaveInput) (*mcp.CallToolResult, *LeaveOutput, error) {
	stream, err := d.Leave(ctx)
	if err != nil {
		return nil, nil, wrapHint(err)
	}
	req := &rpc.LeaveRequest{Namespace: in.Namespace, Workloads: in.Workloads, Level: 0}
	if err := stream.Send(req); err != nil {
		return nil, nil, wrapHint(fmt.Errorf("send leave request: %w", err))
	}
	_, msgs, err := drainProgress[rpc.LeaveResponse](stream)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, wrapHint(err)
	}
	return nil, &LeaveOutput{Messages: tail(msgs, 5)}, nil
}

// DisconnectInput ends a VPN session. Empty input disconnects the current session.
type DisconnectInput struct {
	ConnectionID string `json:"connectionId,omitempty" jsonschema:"connection to disconnect; empty = current"`
}

// DisconnectOutput reports the disconnect result.
type DisconnectOutput struct {
	Messages []string `json:"messages"`
}

// disconnectTool is the plain function behind the kubevpn_disconnect tool.
func disconnectTool(ctx context.Context, d daemonAPI, opts Options, in DisconnectInput) (*mcp.CallToolResult, *DisconnectOutput, error) {
	stream, err := d.Disconnect(ctx)
	if err != nil {
		return nil, nil, wrapHint(err)
	}
	req := &rpc.DisconnectRequest{Level: 0}
	if in.ConnectionID != "" {
		req.ConnectionID = &in.ConnectionID
	}
	if err := stream.Send(req); err != nil {
		return nil, nil, wrapHint(fmt.Errorf("send disconnect request: %w", err))
	}
	_, msgs, err := drainProgress[rpc.DisconnectResponse](stream)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, wrapHint(err)
	}
	return nil, &DisconnectOutput{Messages: tail(msgs, 5)}, nil
}
