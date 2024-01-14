package cmds

import (
	"os"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func NewKubeVPNCommand() *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "kubevpn",
		Short: i18n.T("kubevpn connect to Kubernetes cluster network"),
		Long: templates.LongDesc(`
      kubevpn connect to Kubernetes cluster network.
      `),
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help()
		},
	}

	flags := cmd.PersistentFlags()
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.WrapConfigFn = func(c *rest.Config) *rest.Config {
		if path, ok := os.LookupEnv(config.EnvSSHJump); ok {
			kubeconfigBytes, err := os.ReadFile(path)
			cmdutil.CheckErr(err)
			var conf *restclient.Config
			conf, err = clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
			cmdutil.CheckErr(err)
			return conf
		}
		return c
	}
	configFlags.AddFlags(flags)
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	matchVersionFlags.AddFlags(flags)
	factory := cmdutil.NewFactory(matchVersionFlags)

	groups := templates.CommandGroups{
		{
			Message: "Develop commands:",
			Commands: []*cobra.Command{
				CmdConnect(factory),
				CmdDisconnect(factory),
				CmdProxy(factory),
				CmdLeave(factory),
				CmdClone(factory),
				CmdRemove(factory),
				CmdDev(factory),
				// Hidden, Server Commands (DO NOT USE IT !!!)
				CmdControlPlane(factory),
				CmdServe(factory),
				CmdDaemon(factory),
				CmdWebhook(factory),
			},
		},
		{
			Message: "Management commands",
			Commands: []*cobra.Command{
				CmdList(factory),
				CmdGet(factory),
				CmdConfig(factory),
				CmdCp(factory),
				CmdSSH(factory),
				CmdSSHDaemon(factory),
				CmdLogs(factory),
				CmdReset(factory),
				CmdQuit(factory),
			},
		},
		{
			Message: "Other commands",
			Commands: []*cobra.Command{
				CmdStatus(factory),
				CmdVersion(factory),
				CmdUpgrade(factory),
			},
		},
	}
	groups.Add(cmd)
	templates.ActsAsRootCommand(cmd, []string{"options"}, groups...)
	cmd.AddCommand(CmdOptions(factory))
	return cmd
}
