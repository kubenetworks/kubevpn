package cmds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/containerd/containerd/platforms"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/net/websocket"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/kubectl/pkg/util/term"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// CmdSSH
// Remember to use network mask 32, because ssh using unique network CIDR 198.18.0.0/16
func CmdSSH(_ cmdutil.Factory) *cobra.Command {
	var sshConf = &pkgssh.SshConfig{}
	var extraCIDR []string
	var platform string
	var lite bool
	cmd := &cobra.Command{
		Use:   "ssh",
		Short: "Ssh to jump server",
		Long: templates.LongDesc(i18n.T(`
		Ssh to jump server
		`)),
		Example: templates.Examples(i18n.T(`
        # Jump to server behind of bastion host or ssh jump host
		kubevpn ssh --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# It also supports ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────┘
		kubevpn ssh --ssh-alias <alias>

		# Support ssh auth GSSAPI
        kubevpn ssh --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-keytab /path/to/keytab
        kubevpn ssh --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-cache /path/to/cache
        kubevpn ssh --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-password <PASSWORD>
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			plog.InitLoggerForClient()
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			plat, err := platforms.Parse(platform)
			if err != nil {
				return err
			}
			config, err := websocket.NewConfig("ws://test/ws", "http://test")
			if err != nil {
				return err
			}
			fd := int(os.Stdin.Fd())
			if !terminal.IsTerminal(fd) {
				return fmt.Errorf("stdin is not a terminal")
			}
			width, height, err := terminal.GetSize(fd)
			if err != nil {
				return fmt.Errorf("terminal get size: %s", err)
			}
			sessionID := uuid.NewString()
			ssh := handler.Ssh{
				Config:    *sshConf,
				ExtraCIDR: extraCIDR,
				Width:     width,
				Height:    height,
				Platform:  platforms.Format(platforms.Normalize(plat)),
				SessionID: sessionID,
				Lite:      lite,
			}
			marshal, err := json.Marshal(ssh)
			if err != nil {
				return err
			}
			config.Header.Set("ssh", string(marshal))
			client := daemon.GetTCPClient(true)
			if client == nil {
				return fmt.Errorf("client is nil")
			}
			conn, err := websocket.NewClient(config, client)
			if err != nil {
				return err
			}
			defer conn.Close()

			errChan := make(chan error, 3)
			go func() {
				errChan <- monitorSize(cmd.Context(), sessionID)
			}()

			readyCtx, cancelFunc := context.WithCancel(cmd.Context())
			checker := func(log string) bool {
				isReady := strings.Contains(log, fmt.Sprintf(handler.SshTerminalReadyFormat, sessionID))
				if isReady {
					cancelFunc()
				}
				return isReady
			}
			var state *terminal.State
			go func() {
				select {
				case <-cmd.Context().Done():
					return
				case <-readyCtx.Done():
				}
				if state, err = terminal.MakeRaw(fd); err != nil {
					plog.G(context.Background()).Errorf("terminal make raw: %s", err)
				}
			}()

			go func() {
				_, err := io.Copy(conn, os.Stdin)
				errChan <- err
			}()
			go func() {
				_, err := io.Copy(io.MultiWriter(os.Stdout, util.NewWriter(checker)), conn)
				errChan <- err
			}()

			defer func() {
				if state != nil {
					terminal.Restore(fd, state)
				}
			}()

			select {
			case err := <-errChan:
				return err
			case <-cmd.Context().Done():
				return cmd.Context().Err()
			}
		},
	}
	pkgssh.AddSshFlags(cmd.Flags(), sshConf)
	cmd.Flags().StringArrayVar(&extraCIDR, "extra-cidr", []string{}, "Extra network CIDR string, eg: --extra-cidr 192.168.0.159/24 --extra-cidr 192.168.1.160/32")
	cmd.Flags().StringVar(&platform, "platform", util.If(os.Getenv("KUBEVPN_DEFAULT_PLATFORM") != "", os.Getenv("KUBEVPN_DEFAULT_PLATFORM"), "linux/amd64"), "Set ssh server platform if needs to install command kubevpn")
	cmd.Flags().BoolVar(&lite, "lite", false, "connect to ssh server in lite mode. mode \"lite\": design for only connect to ssh server. mode \"full\": not only connect to ssh server, it also create a two-way tunnel communicate with inner ip")
	return cmd
}

func monitorSize(ctx context.Context, sessionID string) error {
	conn := daemon.GetTCPClient(true)
	if conn == nil {
		return fmt.Errorf("conn is nil")
	}
	var tt = term.TTY{
		In:     os.Stdin,
		Out:    os.Stdout,
		Raw:    false,
		TryDev: false,
		Parent: nil,
	}
	sizeQueue := tt.MonitorSize(tt.GetSize())
	if sizeQueue == nil {
		return fmt.Errorf("sizeQueue is nil")
	}
	//defer runtime.HandleCrash()
	config, err := websocket.NewConfig("ws://test/resize", "http://test")
	if err != nil {
		return err
	}
	config.Header.Set("session-id", sessionID)
	client, err := websocket.NewClient(config, conn)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(client)
	for ctx.Err() == nil {
		size := sizeQueue.Next()
		if size == nil {
			return nil
		}
		if err = encoder.Encode(&size); err != nil {
			plog.G(ctx).Errorf("Encode resize: %s", err)
			return err
		}
	}
	return nil
}
