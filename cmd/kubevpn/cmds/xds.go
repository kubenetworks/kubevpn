package cmds

import (
	"context"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	"github.com/wencaiwulue/kubevpn/v2/pkg/xds"
)

func CmdXDS(f cmdutil.Factory) *cobra.Command {
	var port uint = 9002
	cmd := &cobra.Command{
		Use:    "xds",
		Hidden: true,
		Short:  i18n.T("xds is a envoy xDS server"),
		Long: templates.LongDesc(i18n.T(`
		xds is a envoy xDS server, distribute envoy route configuration
		`)),
		PreRun: func(*cobra.Command, []string) {
			go util.StartupPProfForServer(0)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return xds.Main(cmd.Context(), f, port, plog.G(context.Background()))
		},
	}
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "true/false")
	return cmd
}
