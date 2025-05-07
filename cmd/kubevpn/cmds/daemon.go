package cmds

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdDaemon(cmdutil.Factory) *cobra.Command {
	var opt = &daemon.SvrOption{}
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: i18n.T("Startup kubevpn daemon server"),
		Long:  templates.LongDesc(i18n.T(`Startup kubevpn daemon server`)),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return err
			}
			opt.ID = base64.URLEncoding.EncodeToString(b)

			if opt.IsSudo {
				go util.StartupPProf(config.SudoPProfPort)
				_ = os.RemoveAll("/etc/resolver")
				_ = dns.CleanupHosts()
				_ = util.CleanupTempKubeConfigFile()
			} else {
				go util.StartupPProf(config.PProfPort)
			}
			return initLogfile(config.GetDaemonLogPath(opt.IsSudo))
		},
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			defer opt.Stop()
			defer func() {
				if errors.Is(err, http.ErrServerClosed) {
					err = nil
				}
				if opt.IsSudo {
					for _, profile := range pprof.Profiles() {
						func() {
							file, e := os.Create(filepath.Join(config.GetPProfPath(), profile.Name()))
							if e != nil {
								return
							}
							defer file.Close()
							e = profile.WriteTo(file, 1)
							if e != nil {
								return
							}
						}()
					}
				}
			}()
			return opt.Start(cmd.Context())
		},
		Hidden:                true,
		DisableFlagsInUseLine: true,
	}
	cmd.Flags().BoolVar(&opt.IsSudo, "sudo", false, "is sudo or not")
	return cmd
}

func initLogfile(path string) error {
	_, err := os.Lstat(path)
	if os.IsNotExist(err) {
		var f *os.File
		f, err = os.Create(path)
		if err != nil {
			return err
		}
		_ = f.Close()
		return os.Chmod(path, 0644)
	}
	return nil
}
