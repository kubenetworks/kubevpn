package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/containerd/containerd/platforms"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
	"k8s.io/client-go/tools/remotecommand"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
)

const SshTerminalReadyFormat = "Enter terminal %s"

type wsHandler struct {
	ctx       context.Context
	conn      *websocket.Conn
	sshConfig *pkgssh.SshConfig
	cidr      []string
	width     int
	height    int
	sessionId string
	platform  specs.Platform
	condReady context.CancelFunc
}

// handle orchestrates the SSH workflow:
// 1) optionally install kubevpn and create tunnel
// 2) open an interactive terminal session
func (w *wsHandler) handle(lite bool) {
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()

	cli, err := pkgssh.DialSshRemote(ctx, w.sshConfig, ctx.Done())
	if err != nil {
		w.log("Dial ssh remote error: %v", err)
		return
	}
	defer cli.Close()

	if !lite {
		if err := w.createTunnel(ctx, cli); err != nil {
			return
		}
	}

	rw := newConnWatcher(w.conn)
	go func() {
		<-rw.closed()
		cancel()
	}()

	if err := w.terminal(ctx, cli, rw); err != nil {
		w.log("Enter terminal error: %v", err)
	}
}

func (w *wsHandler) terminal(ctx context.Context, cli *ssh.Client, conn io.ReadWriter) error {
	session, err := cli.NewSession()
	if err != nil {
		w.log("New session error: %v", err)
		return err
	}
	defer session.Close()
	go func() {
		<-ctx.Done()
		session.Close()
	}()
	session.Stdout = conn
	session.Stderr = conn
	session.Stdin = conn

	sessionRegistry.storeSession(w.sessionId, session)
	w.condReady()

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.ECHOCTL:       0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err = session.RequestPty("xterm-256color", w.height, w.width, modes); err != nil {
		w.log("Request pty error: %v", err)
		return err
	}
	if err = session.Shell(); err != nil {
		w.log("Start shell error: %v", err)
		return err
	}
	w.log(SshTerminalReadyFormat, w.sessionId)
	return session.Wait()
}

func (w *wsHandler) log(format string, a ...any) {
	str := format
	if len(a) != 0 {
		str = fmt.Sprintf(format, a...)
	}
	w.conn.Write([]byte(str + "\r\n"))
	plog.G(w.ctx).Infof(format, a...)
}

// Ssh is the configuration received from the websocket client.
type Ssh struct {
	Config    pkgssh.SshConfig
	ExtraCIDR []string
	Width     int
	Height    int
	Platform  string
	SessionID string
	Lite      bool
}

func init() {
	http.Handle("/ws", websocket.Handler(func(conn *websocket.Conn) {
		b := conn.Request().Header.Get("ssh")
		var conf Ssh
		if err := json.Unmarshal([]byte(b), &conf); err != nil {
			conn.Write([]byte(err.Error()))
			conn.Close()
			return
		}
		defer sessionRegistry.cleanup(conf.SessionID)

		ctx, cancelFunc := context.WithCancel(conn.Request().Context())
		defer cancelFunc()

		// readyCtx is an independent signal that the terminal is ready; it must NOT
		// share the session's working context, otherwise signalling readiness would
		// also tear the session down (closing it mid-handshake).
		readyCtx, readyCancel := context.WithCancel(context.Background())
		defer readyCancel()

		h := &wsHandler{
			ctx:       ctx,
			sshConfig: &conf.Config,
			conn:      conn,
			cidr:      conf.ExtraCIDR,
			width:     conf.Width,
			height:    conf.Height,
			sessionId: conf.SessionID,
			platform:  platforms.MustParse(conf.Platform),
			condReady: readyCancel,
		}
		sessionRegistry.storeReady(conf.SessionID, readyCtx)
		defer conn.Close()
		h.handle(conf.Lite)
	}))

	http.Handle("/resize", websocket.Handler(func(conn *websocket.Conn) {
		ctx := conn.Request().Context()
		sessionID := conn.Request().Header.Get("session-id")
		plog.G(ctx).Infof("Resize: %s", sessionID)
		defer conn.Close()

		readyCtx, ok := sessionRegistry.loadReady(sessionID)
		if !ok {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-readyCtx.Done():
		}

		session, ok := sessionRegistry.loadSession(sessionID)
		if !ok {
			return
		}

		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if errors.Is(err, io.EOF) {
				return
			} else if err != nil {
				plog.G(ctx).Errorf("Failed to read resize event for session %s: %v", sessionID, err)
				return
			}
			var size remotecommand.TerminalSize
			if err = json.Unmarshal([]byte(line), &size); err != nil {
				plog.G(ctx).Errorf("Unmarshal terminal size failed: %v", err)
				continue
			}
			if err = session.WindowChange(int(size.Height), int(size.Width)); err != nil {
				if errors.Is(err, io.EOF) {
					return
				}
				plog.G(ctx).Errorf("Session %s window change failed: %v", sessionID, err)
			}
		}
	}))
}
