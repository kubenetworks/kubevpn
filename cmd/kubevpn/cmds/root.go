package cmds

import (
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

func NewKubeVPNCommand() *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "kubevpn",
		Short: "kubevpn",
		Long:  `kubevpn`,
	}

	flags := cmd.PersistentFlags()
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.AddFlags(flags)
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	matchVersionFlags.AddFlags(flags)
	factory := cmdutil.NewFactory(matchVersionFlags)

	cmd.AddCommand(CmdConnect(factory))
	cmd.AddCommand(CmdReset(factory))
	cmd.AddCommand(CmdControlPlane(factory))
	cmd.AddCommand(CmdServe(factory))
	cmd.AddCommand(CmdUpgrade(factory))
	cmd.AddCommand(CmdWebhook(factory))
	cmd.AddCommand(CmdVersion(factory))
	return cmd
}
