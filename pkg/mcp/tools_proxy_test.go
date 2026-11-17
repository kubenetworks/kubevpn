package mcp

import (
	"context"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// proxyStream replays ProxyResponses then EOF; records the sent request.
type proxyStream struct {
	grpc.ClientStream
	grpcSendErr error
	sent        []*rpc.ProxyRequest
	msgs        []string
	err         error
}

func (f *proxyStream) Send(req *rpc.ProxyRequest) error {
	if f.grpcSendErr != nil {
		return f.grpcSendErr
	}
	f.sent = append(f.sent, req)
	return nil
}

func (f *proxyStream) Recv() (*rpc.ProxyResponse, error) {
	m := new(rpc.ProxyResponse)
	if err := f.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

func (f *proxyStream) RecvMsg(m any) error {
	if len(f.msgs) == 0 {
		if f.err != nil {
			return f.err
		}
		return io.EOF
	}
	if mm, ok := m.(*rpc.ProxyResponse); ok {
		mm.Message = f.msgs[0]
		f.msgs = f.msgs[1:]
	}
	return nil
}

type fakeProxyDaemon struct {
	fakeDaemon
	stream *proxyStream
}

func (f *fakeProxyDaemon) Proxy(ctx context.Context, opts ...grpc.CallOption) (rpc.Daemon_ProxyClient, error) {
	return f.stream, nil
}

func TestProxyToolRequiresWorkloads(t *testing.T) {
	_, _, err := proxyTool(context.Background(), &fakeProxyDaemon{stream: &proxyStream{}}, Options{}, ProxyInput{})
	if err == nil || !strings.Contains(err.Error(), "workload") {
		t.Fatalf("expected workload validation error, got %v", err)
	}
}

func TestProxyToolRejectsBareProxyWithoutHeaders(t *testing.T) {
	d := &fakeProxyDaemon{stream: &proxyStream{}}
	_, _, err := proxyTool(context.Background(), d, Options{}, ProxyInput{Workloads: []string{"deploy/web"}, Namespace: "ns"})
	if err == nil || !strings.Contains(err.Error(), "headers") {
		t.Fatalf("expected header-scoping guard, got %v", err)
	}
	if len(d.stream.sent) != 0 {
		t.Fatalf("guard must reject before sending any RPC")
	}
}

func TestProxyToolMapsRequestFields(t *testing.T) {
	d := &fakeProxyDaemon{stream: &proxyStream{msgs: []string{"injected"}}}
	_, out, err := proxyTool(context.Background(), d,
		Options{KubeconfigBytes: []byte("kc")},
		ProxyInput{Workloads: []string{"deploy/web"}, Namespace: "ns", Headers: map[string]string{"X-KubeVPN-User": "agent"}},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sent := d.stream.sent[0]
	if sent.GetNamespace() != "ns" || len(sent.GetWorkloads()) != 1 || sent.GetHeaders()["X-KubeVPN-User"] != "agent" {
		t.Fatalf("request fields not mapped: %+v", sent)
	}
	if len(out.Messages) != 1 || out.Messages[0] != "injected" {
		t.Fatalf("messages not surfaced: %+v", out.Messages)
	}
}

// leaveStream replays LeaveResponses then EOF; records the sent request.
type leaveStream struct {
	grpc.ClientStream
	grpcSendErr error
	sent        []*rpc.LeaveRequest
	msgs        []string
	err         error
}

func (f *leaveStream) Send(req *rpc.LeaveRequest) error {
	if f.grpcSendErr != nil {
		return f.grpcSendErr
	}
	f.sent = append(f.sent, req)
	return nil
}

func (f *leaveStream) Recv() (*rpc.LeaveResponse, error) {
	m := new(rpc.LeaveResponse)
	if err := f.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

func (f *leaveStream) RecvMsg(m any) error {
	if len(f.msgs) == 0 {
		if f.err != nil {
			return f.err
		}
		return io.EOF
	}
	if mm, ok := m.(*rpc.LeaveResponse); ok {
		mm.Message = f.msgs[0]
		f.msgs = f.msgs[1:]
	}
	return nil
}

type fakeLeaveDaemon struct {
	fakeDaemon
	stream *leaveStream
}

func (f *fakeLeaveDaemon) Leave(ctx context.Context, opts ...grpc.CallOption) (rpc.Daemon_LeaveClient, error) {
	return f.stream, nil
}

func newFakeLeaveDaemon(msgs []string) *fakeLeaveDaemon {
	return &fakeLeaveDaemon{stream: &leaveStream{msgs: msgs}}
}

func TestLeaveToolMapsRequestFields(t *testing.T) {
	// fakeLeaveDaemon mirrors fakeProxyDaemon with rpc.LeaveRequest/response.
	d := newFakeLeaveDaemon([]string{"restored"})
	_, out, err := leaveTool(context.Background(), d, Options{}, LeaveInput{Namespace: "ns", Workloads: []string{"deploy/web"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.stream.sent[0].GetNamespace() != "ns" || len(d.stream.sent[0].GetWorkloads()) != 1 {
		t.Fatalf("request fields not mapped: %+v", d.stream.sent[0])
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages not surfaced: %+v", out.Messages)
	}
}

// disconnectStream replays DisconnectResponses then EOF; records the sent request.
type disconnectStream struct {
	grpc.ClientStream
	grpcSendErr error
	sent        []*rpc.DisconnectRequest
	msgs        []string
	err         error
}

func (f *disconnectStream) Send(req *rpc.DisconnectRequest) error {
	if f.grpcSendErr != nil {
		return f.grpcSendErr
	}
	f.sent = append(f.sent, req)
	return nil
}

func (f *disconnectStream) Recv() (*rpc.DisconnectResponse, error) {
	m := new(rpc.DisconnectResponse)
	if err := f.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

func (f *disconnectStream) RecvMsg(m any) error {
	if len(f.msgs) == 0 {
		if f.err != nil {
			return f.err
		}
		return io.EOF
	}
	if mm, ok := m.(*rpc.DisconnectResponse); ok {
		mm.Message = f.msgs[0]
		f.msgs = f.msgs[1:]
	}
	return nil
}

type fakeDisconnectDaemon struct {
	fakeDaemon
	stream *disconnectStream
}

func (f *fakeDisconnectDaemon) Disconnect(ctx context.Context, opts ...grpc.CallOption) (rpc.Daemon_DisconnectClient, error) {
	return f.stream, nil
}

func newFakeDisconnectDaemon() *fakeDisconnectDaemon {
	return &fakeDisconnectDaemon{stream: &disconnectStream{}}
}

func TestDisconnectToolDefaultsToCurrent(t *testing.T) {
	d := newFakeDisconnectDaemon()
	_, _, err := disconnectTool(context.Background(), d, Options{}, DisconnectInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.stream.sent[0].ConnectionID != nil {
		t.Fatalf("empty input must leave ConnectionID unset (daemon defaults to current)")
	}
}

func TestDisconnectToolByID(t *testing.T) {
	d := newFakeDisconnectDaemon()
	id := "conn-9"
	_, _, err := disconnectTool(context.Background(), d, Options{}, DisconnectInput{ConnectionID: id})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.stream.sent[0].ConnectionID == nil || *d.stream.sent[0].ConnectionID != id {
		t.Fatalf("want ConnectionID=%q, got %+v", id, d.stream.sent[0])
	}
}
