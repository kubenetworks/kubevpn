package cmds

import (
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

func NewKubeVPNCommand() *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "kubevpn",
		Short: i18n.T("kubevpn connect to Kubernetes cluster network"),
		Long: templates.LongDesc(`
      kubevpn connect to Kubernetes cluster network.
      `),
	}

	flags := cmd.PersistentFlags()
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.AddFlags(flags)
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	matchVersionFlags.AddFlags(flags)
	factory := cmdutil.NewFactory(matchVersionFlags)

	groups := templates.CommandGroups{
		{
			Message: "Client Commands:",
			Commands: []*cobra.Command{
				CmdConnect(factory),
				CmdProxy(factory),
				CmdDev(factory),
				CmdDuplicate(factory),
				CmdReset(factory),
				CmdUpgrade(factory),
				CmdVersion(factory),
				CmdOptions(factory),
				CmdCp(factory),
			},
		},
		{
			Message: "Server Commands (DO NOT USE IT !!!):",
			Commands: []*cobra.Command{
				CmdControlPlane(factory),
				CmdServe(factory),
				CmdWebhook(factory),
			},
		},
	}
	groups.Add(cmd)
	templates.ActsAsRootCommand(cmd, []string{"options"}, groups...)
	return cmd
}
