package cmds

import (
	"context"
	"time"

	"github.com/docker/docker/libnetwork/resolvconf"
	miekgdns "github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdControlPlane(_ cmdutil.Factory) *cobra.Command {
	var (
		watchDirectoryFilename string
		port                   uint = 9002
	)
	cmd := &cobra.Command{
		Use:    "control-plane",
		Hidden: true,
		Short:  i18n.T("Control-plane is a envoy xds server"),
		Long: templates.LongDesc(i18n.T(`
		Control-plane is a envoy xds server, distribute envoy route configuration
		`)),
		RunE: func(cmd *cobra.Command, args []string) error {
			util.InitLoggerForServer(config.Debug)
			go util.StartupPProfForServer(0)
			go func(ctx context.Context) {
				conf, err := miekgdns.ClientConfigFromFile(resolvconf.Path())
				if err != nil {
					return
				}
				for ctx.Err() == nil {
					dns.ListenAndServe("udp", ":53", conf)
					time.Sleep(time.Second * 5)
				}
			}(cmd.Context())
			err := controlplane.Main(cmd.Context(), watchDirectoryFilename, port, log.StandardLogger())
			return err
		},
	}
	cmd.Flags().StringVarP(&watchDirectoryFilename, "watchDirectoryFilename", "w", "/etc/envoy/envoy-config.yaml", "full path to directory to watch for files")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "true/false")
	return cmd
}
