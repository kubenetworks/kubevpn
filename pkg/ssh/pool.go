package ssh

import (
	"context"
	"net"
	"net/netip"
	"sync"

	"github.com/google/uuid"
	gossh "golang.org/x/crypto/ssh"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func newSshClientWrap(client *gossh.Client, cancel context.CancelFunc) *sshClientWrap {
	return &sshClientWrap{Client: client, cancel: cancel}
}

type sshClientWrap struct {
	cancel context.CancelFunc
	*gossh.Client
}

func (c *sshClientWrap) Close() error {
	c.cancel()
	return c.Client.Close()
}

func getRemoteConn(ctx context.Context, clientMap *sync.Map, conf *SshConfig, remote netip.AddrPort) (net.Conn, error) {
	var conn net.Conn
	var err error
	clientMap.Range(func(key, value any) bool {
		cli := value.(*sshClientWrap)
		ctx1, cancelFunc1 := context.WithTimeout(ctx, sshOpTimeout)
		conn, err = cli.DialContext(ctx1, "tcp", remote.String())
		cancelFunc1()
		if err != nil {
			plog.G(ctx).Debugf("Failed to dial remote address %s: %v", remote.String(), err)
			clientMap.Delete(key)
			plog.G(ctx).Error("Delete invalid ssh client from map")
			_ = cli.Close()
			return true
		}
		return false
	})
	if conn != nil {
		return conn, nil
	}

	ctx1, cancelFunc1 := context.WithCancel(ctx)
	var client *gossh.Client
	client, err = DialSshRemote(ctx1, conf, ctx1.Done())
	if err != nil {
		plog.G(ctx).Debugf("Failed to dial remote ssh server: %v", err)
		cancelFunc1()
		return nil, err
	}
	key := uuid.NewString()
	wrap := newSshClientWrap(client, cancelFunc1)
	clientMap.Store(key, wrap)
	go func() {
		<-ctx.Done()
		if val, loaded := clientMap.LoadAndDelete(key); loaded {
			val.(*sshClientWrap).Close()
		}
	}()
	plog.G(ctx1).Debug("Connected to remote ssh server")

	ctx2, cancelFunc2 := context.WithTimeout(ctx, sshOpTimeout)
	defer cancelFunc2()
	conn, err = client.DialContext(ctx2, "tcp", remote.String())
	if err != nil {
		plog.G(ctx).Debugf("Failed to dial remote addr %s: %v", remote.String(), err)
		return nil, err
	}
	plog.G(ctx).Debugf("Connected to remote addr %s", remote.String())
	return conn, nil
}
