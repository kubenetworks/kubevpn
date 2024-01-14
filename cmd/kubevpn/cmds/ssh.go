package cmds

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/net/websocket"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/kubectl/pkg/util/term"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// CmdSSH
// 设置本地的IP是223.254.0.1/32 ，记得一定是掩码 32位，
// 这样别的路由不会走到这里来
func CmdSSH(_ cmdutil.Factory) *cobra.Command {
	var sshConf = &util.SshConfig{}
	var ExtraCIDR []string
	cmd := &cobra.Command{
		Use:   "ssh",
		Short: "Ssh to jump server",
		Long:  `Ssh to jump server`,
		Example: templates.Examples(i18n.T(`
        # Jump to server behind of bastion host or ssh jump host
		kubevpn ssh --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# It also support ProxyJump, like
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
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := websocket.NewConfig("ws://test/ws", "http://test")
			if err != nil {
				return err
			}
			fd := int(os.Stdin.Fd())
			if !terminal.IsTerminal(fd) {
				return fmt.Errorf("stdin is not a terminal")
			}
			state, err := terminal.MakeRaw(fd)
			if err != nil {
				return fmt.Errorf("terminal make raw: %s", err)
			}
			defer terminal.Restore(fd, state)
			width, height, err := terminal.GetSize(fd)
			if err != nil {
				return fmt.Errorf("terminal get size: %s", err)
			}
			sessionID := uuid.NewString()
			config.Header.Set("ssh-addr", sshConf.Addr)
			config.Header.Set("ssh-username", sshConf.User)
			config.Header.Set("ssh-password", sshConf.Password)
			config.Header.Set("ssh-keyfile", sshConf.Keyfile)
			config.Header.Set("ssh-alias", sshConf.ConfigAlias)
			config.Header.Set("extra-cidr", strings.Join(ExtraCIDR, ","))
			config.Header.Set("gssapi-password", sshConf.GSSAPIPassword)
			config.Header.Set("gssapi-keytab", sshConf.GSSAPIKeytabConf)
			config.Header.Set("gssapi-cache", sshConf.GSSAPICacheFile)
			config.Header.Set("terminal-width", strconv.Itoa(width))
			config.Header.Set("terminal-height", strconv.Itoa(height))
			config.Header.Set("session-id", sessionID)
			client := daemon.GetTCPClient(true)
			conn, err := websocket.NewClient(config, client)
			if err != nil {
				return err
			}
			defer conn.Close()
			go moniteSize(sessionID)
			go io.Copy(conn, os.Stdin)
			_, err = io.Copy(os.Stdout, conn)
			return err
		},
	}
	addSshFlags(cmd, sshConf)
	cmd.Flags().StringArrayVar(&ExtraCIDR, "extra-cidr", []string{}, "Extra cidr string, eg: --extra-cidr 192.168.0.159/24 --extra-cidr 192.168.1.160/32")
	return cmd
}

func moniteSize(sessionID string) error {
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
	for {
		size := sizeQueue.Next()
		if size == nil {
			return nil
		}
		if err = encoder.Encode(&size); err != nil {
			log.Errorf("Encode resize: %s", err)
			continue
		}
	}
}
