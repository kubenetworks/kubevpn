package cmds

import (
	"context"

	"github.com/docker/docker/libnetwork/resolvconf"
	miekgdns "github.com/miekg/dns"
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdControlPlane(f cmdutil.Factory) *cobra.Command {
	var port uint = 9002
	cmd := &cobra.Command{
		Use:    "control-plane",
		Hidden: true,
		Short:  i18n.T("Control-plane is a envoy xds server"),
		Long: templates.LongDesc(i18n.T(`
		Control-plane is a envoy xds server, distribute envoy route configuration
		`)),
		RunE: func(cmd *cobra.Command, args []string) error {
			go util.StartupPProfForServer(0)
			go func() {
				conf, err := miekgdns.ClientConfigFromFile(resolvconf.Path())
				if err != nil {
					plog.G(context.Background()).Fatal(err)
				}
				plog.G(context.Background()).Fatal(dns.ListenAndServe("udp", ":53", conf))
			}()
			err := controlplane.Main(cmd.Context(), f, port, plog.G(context.Background()))
			return err
		},
	}
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "true/false")
	return cmd
}
