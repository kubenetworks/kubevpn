package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// StatusInput filters the status query.
type StatusInput struct {
	ConnectionID string `json:"connectionId,omitempty" jsonschema:"connection ID to query; empty lists all connections"`
}

// StatusOutput is the daemon's Status response serialized for the agent.
type StatusOutput struct {
	Result string `json:"result" jsonschema:"daemon StatusResponse as JSON"`
}

// statusTool is the plain function behind the kubevpn_status tool.
func statusTool(ctx context.Context, d daemonAPI, in StatusInput) (*mcp.CallToolResult, *StatusOutput, error) {
	var connIDs []string
	if in.ConnectionID != "" {
		connIDs = []string{in.ConnectionID}
	}
	resp, err := d.Status(ctx, &rpc.StatusRequest{ConnectionIDs: connIDs})
	if err != nil {
		return nil, nil, wrapHint(err)
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal status: %w", err)
	}
	out := &StatusOutput{Result: string(b)}
	return nil, out, nil
}

// LogsInput selects a bounded log tail.
type LogsInput struct {
	Lines        int32  `json:"lines,omitempty" jsonschema:"number of trailing lines to return (default 100)"`
	ConnectionID string `json:"connectionId,omitempty" jsonschema:"connection ID to read logs for; empty = current"`
}

// LogsOutput is the log tail serialized for the agent.
type LogsOutput struct {
	Result string `json:"result" jsonschema:"{lines: [...]} JSON"`
}

// logsTool reads at most in.Lines trailing log lines (non-follow). The bound
// is enforced by counting, not by a deadline: the fake and the real client
// both terminate on EOF, and Lines caps the collected output.
func logsTool(ctx context.Context, d daemonAPI, in LogsInput) (*mcp.CallToolResult, *LogsOutput, error) {
	if in.Lines <= 0 {
		in.Lines = 100
	}
	stream, err := d.Logs(ctx)
	if err != nil {
		return nil, nil, wrapHint(err)
	}
	// Send the log request first
	if err := stream.Send(&rpc.LogRequest{
		Follow:       false,
		Lines:        in.Lines,
		ConnectionID: in.ConnectionID,
	}); err != nil {
		return nil, nil, wrapHint(err)
	}
	var allLines []string
	for {
		resp, err := stream.Recv()
		if err != nil {
			break // io.EOF or terminal error: return what we collected
		}
		allLines = append(allLines, resp.GetMessage())
	}
	// Take the last in.Lines lines
	var lines []string
	if len(allLines) > int(in.Lines) {
		lines = allLines[len(allLines)-int(in.Lines):]
	} else {
		lines = allLines
	}
	b, err := json.Marshal(map[string]any{"lines": lines})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal logs: %w", err)
	}
	out := &LogsOutput{Result: string(b)}
	return nil, out, nil
}

// VersionInput takes no parameters.
type VersionInput struct{}

// VersionOutput reports both the MCP server (client) version and the daemon's.
type VersionOutput struct {
	Result string `json:"result" jsonschema:"{client, daemon} JSON"`
}

// versionTool reports client + daemon versions so an agent can detect skew.
func versionTool(ctx context.Context, d daemonAPI, opts Options, in VersionInput) (*mcp.CallToolResult, *VersionOutput, error) {
	resp, err := d.Version(ctx, &rpc.VersionRequest{})
	if err != nil {
		return nil, nil, wrapHint(err)
	}
	b, err := json.Marshal(map[string]string{"client": opts.Version, "daemon": resp.GetVersion()})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal version: %w", err)
	}
	out := &VersionOutput{Result: string(b)}
	return nil, out, nil
}
