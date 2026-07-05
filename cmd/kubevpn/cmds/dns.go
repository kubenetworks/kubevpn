package cmds

import (
	"github.com/docker/docker/libnetwork/resolvconf"
	miekgdns "github.com/miekg/dns"
	"github.com/spf13/cobra"
	log "github.com/sirupsen/logrus"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdDNS(cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "dns",
		Hidden: true,
		Short:  i18n.T("DNS forward server"),
		Long: templates.LongDesc(i18n.T(`
		DNS forward server, resolve DNS queries using upstream cluster DNS servers.
		`)),
		PreRun: func(*cobra.Command, []string) {
			go util.StartupPProfForServer(0)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// dns logs via the global logger; honor --debug (the dns container is deployed with
			// --debug so it defaults to Debug, consistent with the other server-side components).
			plog.L.SetLevel(log.Level(util.If(config.Debug, log.DebugLevel, log.InfoLevel)))
			conf, err := miekgdns.ClientConfigFromFile(resolvconf.Path())
			if err != nil {
				return err
			}
			return dns.ListenAndServe("udp", ":53", conf)
		},
	}
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "Enable debug log or not")
	return cmd
}
