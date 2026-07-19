package ssh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// TestCopyStream_BidirectionalAndClose verifies copyStream shuttles bytes both
// ways and tears both ends down when one side closes.
func TestCopyStream_BidirectionalAndClose(t *testing.T) {
	// local side of the app <-> tunnel; remote side <-> echo server.
	appConn, localConn := net.Pipe()
	remoteConn, echoConn := net.Pipe()

	go copyStream(context.Background(), localConn, remoteConn)

	// Echo server: read one message, write it back uppercased-length prefixed.
	go func() {
		buf := make([]byte, 64)
		n, err := echoConn.Read(buf)
		if err != nil {
			return
		}
		_, _ = echoConn.Write(buf[:n])
	}()

	msg := []byte("ping")
	done := make(chan error, 1)
	go func() {
		if _, err := appConn.Write(msg); err != nil {
			done <- err
			return
		}
		got := make([]byte, len(msg))
		_, err := io.ReadFull(appConn, got)
		if err != nil {
			done <- err
			return
		}
		if !bytes.Equal(got, msg) {
			done <- errors.New("echo mismatch")
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("round trip failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for echo round trip")
	}

	// Closing the echo end must cascade: appConn eventually sees EOF/closed.
	_ = echoConn.Close()
	_ = appConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := appConn.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected app connection to close after remote closed")
	}
}

// TestCopyStream_ContextCancelClosesBoth verifies ctx cancellation tears down both ends.
func TestCopyStream_ContextCancelClosesBoth(t *testing.T) {
	_, localConn := net.Pipe()
	remoteConn, remotePeer := net.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	go copyStream(ctx, localConn, remoteConn)

	cancel()

	_ = remotePeer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := remotePeer.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected remote peer to observe closure after context cancel")
	}
}

// TestProbeReachable_UnreachableWrapsError locks the clear diagnostic surfaced
// when the tunnel cannot reach the apiserver.
func TestProbeReachable_UnreachableWrapsError(t *testing.T) {
	fc := &fakeClient{dialErr: context.DeadlineExceeded}
	err := probeReachable(context.Background(), fc, testRemote())
	if err == nil {
		t.Fatal("expected error for unreachable target")
	}
	if !contains(err.Error(), "cannot reach apiserver") {
		t.Fatalf("error %q does not mention the clear cause", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error must wrap the underlying cause, got %v", err)
	}
}

// TestProbeReachable_ReachableOK locks the success path (probe closes the conn).
func TestProbeReachable_ReachableOK(t *testing.T) {
	fc := &fakeClient{dialFunc: func() (net.Conn, error) {
		c, _ := net.Pipe()
		return c, nil
	}}
	if err := probeReachable(context.Background(), fc, testRemote()); err != nil {
		t.Fatalf("probe err = %v, want nil", err)
	}
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}
