package cmds

import (
	"os"
	"path/filepath"
	"strconv"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func CmdDaemon(_ cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:                   "daemon",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T(""),
		Long:                  i18n.T(""),
		Example:               ``,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := config.DaemonPortPath
			err := os.MkdirAll(path, os.ModePerm)
			if err != nil {
				return err
			}
			port := util.GetAvailableTCPPortOrDie()
			if util.IsAdmin() {
				err = os.WriteFile(filepath.Join(path, "sudo_daemon"), []byte(strconv.Itoa(port)), os.ModePerm)
			} else {
				err = os.WriteFile(filepath.Join(path, "daemon"), []byte(strconv.Itoa(port)), os.ModePerm)
			}
			if err != nil {
				return err
			}
			d := &daemon.SvrOption{Port: port}
			err = d.Start(cmd.Context())
			if err != nil {
				log.Fatal(err)
			}
			return nil
		},
	}
	return cmd
}
