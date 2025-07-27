package cmds

import (
	"os"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func NewKubeVPNCommand() *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "kubevpn",
		Short: i18n.T("KubeVPN offers a Cloud-Native Dev Environment that seamlessly connects to your Kubernetes cluster network."),
		Long: templates.LongDesc(`
        KubeVPN offers a Cloud-Native Dev Environment that seamlessly connects to your Kubernetes cluster network.
        `),
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help()
		},
	}

	flags := cmd.PersistentFlags()
	configFlags := genericclioptions.NewConfigFlags(true)
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
	matchVersionFlags := cmdutil.NewMatchVersionFlags(&warp{ConfigFlags: configFlags})
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
				CmdSync(factory),
				CmdUnsync(factory),
				CmdDev(factory),
				CmdConnection(factory),
				// Hidden, Server Commands (DO NOT USE IT !!!)
				CmdControlPlane(factory),
				CmdServer(factory),
				CmdDaemon(factory),
				CmdWebhook(factory),
				CmdSyncthing(factory),
				CmdOnce(factory),
			},
		},
		{
			Message: "Management commands:",
			Commands: []*cobra.Command{
				CmdStatus(factory),
				CmdAlias(factory),
				CmdConnection(factory),
				CmdSSH(factory),
				CmdSSHDaemon(factory),
				CmdImageCopy(factory),
				CmdLogs(factory),
				CmdReset(factory),
				CmdUninstall(factory),
				CmdQuit(factory),
			},
		},
		{
			Message: "Other commands:",
			Commands: []*cobra.Command{
				CmdUpgrade(factory),
				CmdVersion(factory),
			},
		},
	}
	groups.Add(cmd)
	templates.ActsAsRootCommand(cmd, []string{"options"}, groups...)
	cmd.AddCommand(CmdOptions(factory))
	return cmd
}

type warp struct {
	*genericclioptions.ConfigFlags
}

func (f *warp) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	if strings.HasPrefix(ptr.Deref[string](f.KubeConfig, ""), "~") {
		home := homedir.HomeDir()
		f.KubeConfig = ptr.To(strings.Replace(*f.KubeConfig, "~", home, 1))
	}
	return f.ConfigFlags.ToRawKubeConfigLoader()
}
