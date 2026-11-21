package mcp

import (
	"errors"
	"io"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// fakeStream replays pre-set messages then EOF (or err).
type fakeStream struct {
	msgs   []string
	err    error
	called int
}

func (f *fakeStream) RecvMsg(m any) error {
	if f.called < len(f.msgs) {
		if mm, ok := m.(*msgWithMessage); ok {
			mm.Message = f.msgs[f.called]
		}
		f.called++
		return nil
	}
	if f.err != nil {
		return f.err
	}
	return io.EOF
}

type msgWithMessage struct{ Message string }

func (m *msgWithMessage) GetMessage() string { return m.Message }

func TestDrainProgressReturnsMessagesThenEOF(t *testing.T) {
	s := &fakeStream{msgs: []string{"step 1", "step 2"}}
	last, msgs, err := drainProgress[msgWithMessage](s)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF on success, got %v", err)
	}
	if last != "step 2" || len(msgs) != 2 {
		t.Fatalf("got last=%q msgs=%v, want last=step 2, 2 messages", last, msgs)
	}
}

func TestDrainProgressPropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	_, _, err := drainProgress[msgWithMessage](&fakeStream{err: sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}
}

func TestDaemonClientSatisfiesInterface(t *testing.T) {
	var _ daemonAPI = rpc.DaemonClient(nil)
}
