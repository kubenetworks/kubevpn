package mcp

import (
	"context"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

type fakeDaemon struct {
	rpc.DaemonClient // embed: unimplemented methods panic if called — pins the narrow surface

	version    *rpc.VersionResponse
	versionErr error
	status     *rpc.StatusResponse
	statusErr  error
	logsStream *fakeStream
	logsErr    error
}

func (f *fakeDaemon) Version(ctx context.Context, in *rpc.VersionRequest, opts ...grpc.CallOption) (*rpc.VersionResponse, error) {
	return f.version, f.versionErr
}

func (f *fakeDaemon) Status(ctx context.Context, in *rpc.StatusRequest, opts ...grpc.CallOption) (*rpc.StatusResponse, error) {
	return f.status, f.statusErr
}

func (f *fakeDaemon) Logs(ctx context.Context, opts ...grpc.CallOption) (rpc.Daemon_LogsClient, error) {
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	return &fakeLogsClient{stream: f.logsStream}, nil
}

type fakeLogsClient struct {
	grpc.ClientStream
	stream *fakeStream
}

func (c *fakeLogsClient) RecvMsg(m any) error {
	if mm, ok := m.(*rpc.LogResponse); ok && c.stream != nil {
		if c.stream.called < len(c.stream.msgs) {
			mm.Message = c.stream.msgs[c.stream.called]
			c.stream.called++
			return nil
		}
		if c.stream.err != nil {
			return c.stream.err
		}
		return io.EOF
	}
	if c.stream != nil {
		if c.stream.called < len(c.stream.msgs) {
			if mm, ok := m.(*msgWithMessage); ok {
				mm.Message = c.stream.msgs[c.stream.called]
			}
			c.stream.called++
			return nil
		}
		if c.stream.err != nil {
			return c.stream.err
		}
		return io.EOF
	}
	return io.EOF
}

func (c *fakeLogsClient) Recv() (*rpc.LogResponse, error) {
	m := new(rpc.LogResponse)
	if err := c.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

func (c *fakeLogsClient) Send(*rpc.LogRequest) error { return nil }

func TestStatusToolPassthrough(t *testing.T) {
	d := &fakeDaemon{status: &rpc.StatusResponse{CurrentConnectionID: "c1"}}
	_, out, err := statusTool(context.Background(), d, StatusInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Result, `"c1"`) {
		t.Fatalf("expected daemon JSON passthrough, got: %s", out.Result)
	}
}

func TestStatusToolSurfacesError(t *testing.T) {
	d := &fakeDaemon{statusErr: io.ErrClosedPipe}
	_, _, err := statusTool(context.Background(), d, StatusInput{})
	if err == nil || !strings.Contains(err.Error(), "hint") {
		t.Fatalf("expected error with hint, got %v", err)
	}
}

func TestLogsToolBoundedTail(t *testing.T) {
	d := &fakeDaemon{logsStream: &fakeStream{msgs: []string{"l1", "l2", "l3"}}}
	_, out, err := logsTool(context.Background(), d, LogsInput{Lines: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Result, "l2") || !strings.Contains(out.Result, "l3") || strings.Contains(out.Result, "l1") {
		t.Fatalf("expected tail [l2 l3], got: %s", out.Result)
	}
}

func TestVersionToolReportsClientAndDaemon(t *testing.T) {
	d := &fakeDaemon{version: &rpc.VersionResponse{Version: "v2.12.1"}}
	_, out, err := versionTool(context.Background(), d, Options{Version: "v1-test"}, VersionInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Result, "v2.12.1") || !strings.Contains(out.Result, "v1-test") {
		t.Fatalf("expected both versions in output, got: %s", out.Result)
	}
}
