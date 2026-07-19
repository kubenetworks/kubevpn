package ssh

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"

	gossh "golang.org/x/crypto/ssh"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// PortMapUntil sets up local TCP port forwarding through an SSH tunnel, forwarding local connections to the remote address.
func PortMapUntil(ctx context.Context, conf *SshConfig, remote, local netip.AddrPort) error {
	// Listen on remote server port
	var lc net.ListenConfig
	localListen, e := lc.Listen(ctx, "tcp", local.String())
	if e != nil {
		plog.G(ctx).Errorf("failed to listen %s: %v", local.String(), e)
		return e
	}
	plog.G(ctx).Debugf("SSH listening on local %s forward to %s", local.String(), remote.String())

	go func() {
		<-ctx.Done()
		localListen.Close()
	}()

	go func() {
		defer localListen.Close()

		clientMap := &sync.Map{}
		ctx1, cancelFunc1 := context.WithCancel(ctx)
		defer cancelFunc1()

		for ctx1.Err() == nil {
			localConn, err1 := localListen.Accept()
			if err1 != nil {
				if errors.Is(err1, net.ErrClosed) {
					return
				}
				plog.G(ctx).Debugf("Failed to accept ssh conn: %v", err1)
				continue
			}
			go func() {
				defer localConn.Close()

				remoteConn, err := getRemoteConn(ctx1, clientMap, conf, remote)
				if err != nil {
					var openChannelError *gossh.OpenChannelError
					// if ssh server not permitted ssh port-forward, do nothing until exit
					if errors.As(err, &openChannelError) && openChannelError.Reason == gossh.Prohibited {
						plog.G(ctx).Errorf("Prohibited to open ssh port-forward to %s: %v", remote.String(), err)
						cancelFunc1()
						return
					}
					plog.G(ctx).Debugf("Failed to dial remote from %s<=>%s -> %s: %v", localConn.LocalAddr().String(), localConn.RemoteAddr().String(), remote.String(), err)
					return
				}
				plog.G(ctx).Debugf("Opened ssh port-forward to %s<=>%s -> %s", localConn.LocalAddr().String(), localConn.RemoteAddr().String(), remote.String())

				defer remoteConn.Close()
				copyStream(ctx, localConn, remoteConn)
			}()
		}
	}()
	return nil
}

func copyStream(ctx context.Context, local net.Conn, remote net.Conn) {
	done := make(chan struct{}, 2)

	copy := func(dst, src net.Conn, direction string) {
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])
		_, err := io.CopyBuffer(dst, src, buf)
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
			plog.G(ctx).Debugf("Copy %s error: %s", direction, err)
		}
		done <- struct{}{}
	}

	go copy(local, remote, "remote->local")
	go copy(remote, local, "local->remote")

	// Wait for either direction to finish or context cancel, then close both
	// connections to unblock the other goroutine.
	select {
	case <-done:
	case <-ctx.Done():
	}
	local.Close()
	remote.Close()
	// Wait for the second goroutine to finish
	<-done
}
