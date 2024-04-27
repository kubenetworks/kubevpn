package cmds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

const (
	FormatJson  = "json"
	FormatYaml  = "yaml"
	FormatTable = "table"
)

func CmdStatus(f cmdutil.Factory) *cobra.Command {
	var aliasName string
	var localFile string
	var remoteAddr string
	var format string
	cmd := &cobra.Command{
		Use:   "status",
		Short: i18n.T("KubeVPN status"),
		Long:  templates.LongDesc(i18n.T(`KubeVPN status`)),
		Example: templates.Examples(i18n.T(`
        # show status for kubevpn status
        kubevpn status

		# query status by alias config name dev_new 
        kubevpn status --alias dev_new
`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var clusterIDs []string
			if aliasName != "" {
				configs, err := ParseAndGet(localFile, remoteAddr, aliasName)
				if err != nil {
					return err
				}
				for _, config := range configs {
					clusterID, err := GetClusterIDByConfig(cmd, config)
					if err != nil {
						return err
					}
					clusterIDs = append(clusterIDs, clusterID)
				}
			}

			resp, err := daemon.GetClient(false).Status(
				cmd.Context(),
				&rpc.StatusRequest{
					ClusterIDs: clusterIDs,
				},
			)
			if err != nil {
				return err
			}
			output, err := genOutput(resp, format)
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, output)
			return nil
		},
	}
	cmd.Flags().StringVar(&aliasName, "alias", "", "Alias name, query connect status by alias config name")
	cmd.Flags().StringVarP(&localFile, "file", "f", daemon.GetConfigFilePath(), "Config file location")
	cmd.Flags().StringVarP(&remoteAddr, "remote", "r", "", "Remote config file, eg: https://raw.githubusercontent.com/kubenetworks/kubevpn/master/pkg/config/config.yaml")
	cmd.Flags().StringVarP(&format, "output", "o", FormatTable, fmt.Sprintf("Output format. One of: (%s, %s, %s)", FormatJson, FormatYaml, FormatTable))
	return cmd
}

func genOutput(status *rpc.StatusResponse, format string) (string, error) {
	switch format {
	case FormatJson:
		marshal, err := json.Marshal(status.List)
		if err != nil {
			return "", err
		}
		return string(marshal), nil

	case FormatYaml:
		marshal, err := yaml.Marshal(status.List)
		if err != nil {
			return "", err
		}
		return string(marshal), nil
	default:
		var sb = new(bytes.Buffer)
		w := tabwriter.NewWriter(sb, 1, 1, 1, ' ', 0)
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", "ID", "Mode", "Cluster", "Kubeconfig", "Namespace", "Status", "Netif")

		for _, s := range status.List {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				s.ID, s.Mode, s.Cluster, s.Kubeconfig, s.Namespace, s.Status, s.Netif)
		}
		_ = w.Flush()
		return sb.String(), nil
	}
}

func GetClusterIDByConfig(cmd *cobra.Command, config Config) (string, error) {
	flags := flag.NewFlagSet("", flag.ContinueOnError)
	var sshConf = &util.SshConfig{}
	addSshFlags(flags, sshConf)
	configFlags := genericclioptions.NewConfigFlags(false).WithDeprecatedPasswordFlag()
	configFlags.AddFlags(flags)
	matchVersionFlags := cmdutil.NewMatchVersionFlags(&warp{ConfigFlags: configFlags})
	matchVersionFlags.AddFlags(flags)
	factory := cmdutil.NewFactory(matchVersionFlags)

	for _, command := range cmd.Parent().Commands() {
		command.Flags().VisitAll(func(f *flag.Flag) {
			if flags.Lookup(f.Name) == nil && flags.ShorthandLookup(f.Shorthand) == nil {
				flags.AddFlag(f)
			}
		})
	}

	split := strings.Split(strings.Join(config.Flags, " "), " ")
	err := flags.ParseAll(split[:], func(flag *flag.Flag, value string) error {
		_ = flags.Set(flag.Name, value)
		return nil
	})
	bytes, ns, err := util.ConvertToKubeConfigBytes(factory)
	if err != nil {
		return "", err
	}
	file, err := util.ConvertToTempKubeconfigFile(bytes)
	if err != nil {
		return "", err
	}
	flags = flag.NewFlagSet("", flag.ContinueOnError)
	flags.AddFlag(&flag.Flag{
		Name:     "kubeconfig",
		DefValue: file,
	})
	flags.AddFlag(&flag.Flag{
		Name:     "namespace",
		DefValue: ns,
	})
	var path string
	path, err = util.SshJump(cmd.Context(), sshConf, flags, false)
	if err != nil {
		return "", err
	}
	var c = &handler.ConnectOptions{}
	err = c.InitClient(util.InitFactoryByPath(path, ns))
	if err != nil {
		return "", err
	}
	err = c.InitDHCP(cmd.Context())
	if err != nil {
		return "", err
	}
	return c.GetClusterID(), nil
}
