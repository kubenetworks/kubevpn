package mcp

import (
	"context"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// fakeBidiStream replays ConnectResponses then EOF; sends are recorded.
type fakeBidiStream struct {
	grpc.ClientStream
	sent   []*rpc.ConnectRequest
	msgIDs []string
	err    error
}

func (f *fakeBidiStream) Send(req *rpc.ConnectRequest) error {
	f.sent = append(f.sent, req)
	return nil
}

func (f *fakeBidiStream) Recv() (*rpc.ConnectResponse, error) {
	m := new(rpc.ConnectResponse)
	if err := f.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

func (f *fakeBidiStream) RecvMsg(m any) error {
	if len(f.msgIDs) == 0 {
		if f.err != nil {
			return f.err
		}
		return io.EOF
	}
	if mm, ok := m.(*rpc.ConnectResponse); ok {
		mm.ConnectionID = f.msgIDs[0]
		f.msgIDs = f.msgIDs[1:]
	}
	return nil
}

type fakeConnectDaemon struct {
	fakeDaemon
	stream *fakeBidiStream
}

func (f *fakeConnectDaemon) Connect(ctx context.Context, opts ...grpc.CallOption) (rpc.Daemon_ConnectClient, error) {
	return f.stream, nil
}

func TestConnectToolReturnsConnectionIDOnEstablished(t *testing.T) {
	d := &fakeConnectDaemon{stream: &fakeBidiStream{msgIDs: []string{"conn-1"}}}
	opts := Options{KubeconfigBytes: []byte("kube"), Namespace: "default", OriginKubeconfigPath: "/tmp/kc"}
	_, out, err := connectTool(context.Background(), d, opts, ConnectInput{Namespace: "ns1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.ConnectionID != "conn-1" {
		t.Fatalf("want conn-1, got %q", out.ConnectionID)
	}
	sent := d.stream.sent[0]
	if sent.GetNamespace() != "ns1" || string(sent.GetKubeconfigBytes()) != "kube" {
		t.Fatalf("request fields not mapped: %+v", sent)
	}
}

func TestConnectToolDefaultsNamespaceFromOptions(t *testing.T) {
	d := &fakeConnectDaemon{stream: &fakeBidiStream{msgIDs: []string{"c"}}}
	_, _, err := connectTool(context.Background(), d, Options{KubeconfigBytes: []byte("k"), Namespace: "opt-ns"}, ConnectInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.stream.sent[0].GetNamespace() != "opt-ns" {
		t.Fatalf("want default namespace opt-ns, got %q", d.stream.sent[0].GetNamespace())
	}
}

func TestConnectToolWrapsFailureWithHint(t *testing.T) {
	d := &fakeConnectDaemon{stream: &fakeBidiStream{err: io.ErrClosedPipe}}
	_, _, err := connectTool(context.Background(), d, Options{KubeconfigBytes: []byte("k")}, ConnectInput{})
	if err == nil || !strings.Contains(err.Error(), "sudo kubevpn daemon") {
		t.Fatalf("expected elevation hint on failure, got %v", err)
	}
}
