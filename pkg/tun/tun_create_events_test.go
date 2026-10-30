//go:build linux || darwin

package tun

import (
	"errors"
	"net"
	"testing"

	"golang.zx2c4.com/wireguard/tun"
)

// Linux overrides this hook to avoid enumerating ambient host interfaces.
var prepareCreateEventsTest = func(t *testing.T) {}

type nameFailureEventDevice struct {
	*eventLifecycleDevice
	nameErr error
}

func (f *nameFailureEventDevice) Name() (string, error) {
	// Name cannot finish until its event producer has been drained. No OS
	// interface lookup or mutation can be reached after this sentinel failure.
	f.stopOnce.Do(func() { close(f.stop) })
	<-f.producerDone
	return "", f.nameErr
}

func TestCreateTunNameFailureDrainsEvents(t *testing.T) {
	prepareCreateEventsTest(t)
	f := &nameFailureEventDevice{
		eventLifecycleDevice: newEventLifecycleDevice(t, "name"),
		nameErr:              errors.New("native name sentinel"),
	}
	original := createNativeTUN
	t.Cleanup(func() { createNativeTUN = original })
	createCalls := 0
	createNativeTUN = func(string, int) (tun.Device, error) {
		createCalls++
		return f, nil
	}
	var conn net.Conn
	var itf *net.Interface
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, itf, err = createTun(Config{Addr: "198.18.0.1/32"})
	}()
	if !waitEventBarrier(t, done, "Name failure cleanup") {
		f.abortOnce.Do(func() { close(f.abort) })
		if !waitEventBarrier(t, done, "aborted creation worker") {
			t.Fatal("creation worker did not terminate")
		}
	}
	if conn != nil || itf != nil || err != f.nameErr {
		t.Fatalf("createTun = (%v, %v, %v), want nil, nil, name sentinel", conn, itf, err)
	}
	if createCalls != 1 || f.eventsCalls.Load() != 1 || f.closeCalls.Load() != 1 {
		t.Errorf("create/Events/Close calls = %d/%d/%d, want 1/1/1", createCalls, f.eventsCalls.Load(), f.closeCalls.Load())
	}
	waitEventBarrier(t, f.producerDone, "Name event producer joined")
}
