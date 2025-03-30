package cmds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/liggitt/tabwriter"
	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/printers"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
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
		Short: i18n.T("Show connect status and list proxy/clone resource"),
		Long: templates.LongDesc(i18n.T(`
		Show connect status and list proxy/clone resource

		Show connect status and list proxy or clone resource, you can check connect status by filed status and netif.
		if netif is empty, means tun device closed, so it's unhealthy, it will also show route info, if proxy workloads, 
		not only show myself proxy resource, another route info will also display.
		`)),
		Example: templates.Examples(i18n.T(`
        # show status for connect status and list proxy/clone resource 
        kubevpn status

		# query status by alias config name dev_new 
        kubevpn status --alias dev_new

		# query status with output json format
		kubevpn status -o json

		# query status with output yaml format
		kubevpn status -o yaml
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) (err error) {
			plog.InitLoggerForClient()
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var clusterIDs []string
			if aliasName != "" {
				configs, err := ParseAndGet(localFile, remoteAddr, aliasName)
				if err != nil {
					return err
				}
				for _, conf := range configs {
					clusterID, err := GetClusterIDByConfig(cmd, conf)
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
			_, _ = fmt.Fprint(os.Stdout, output)
			return nil
		},
	}
	cmd.Flags().StringVar(&aliasName, "alias", "", "Alias name, query connect status by alias config name")
	cmd.Flags().StringVarP(&localFile, "kubevpnconfig", "f", util.If(os.Getenv("KUBEVPNCONFIG") != "", os.Getenv("KUBEVPNCONFIG"), config.GetConfigFilePath()), "Path to the kubevpnconfig file to use for CLI requests.")
	cmd.Flags().StringVarP(&remoteAddr, "remote", "r", "", "Remote config file, eg: https://raw.githubusercontent.com/kubenetworks/kubevpn/master/pkg/config/config.yaml")
	cmd.Flags().StringVarP(&format, "output", "o", FormatTable, fmt.Sprintf("Output format. One of: (%s, %s, %s)", FormatJson, FormatYaml, FormatTable))
	return cmd
}

func genOutput(status *rpc.StatusResponse, format string) (string, error) {
	switch format {
	case FormatJson:
		if len(status.List) == 0 {
			return "", nil
		}
		marshal, err := json.Marshal(status.List)
		if err != nil {
			return "", err
		}
		return string(marshal), nil

	case FormatYaml:
		if len(status.List) == 0 {
			return "", nil
		}
		marshal, err := yaml.Marshal(status.List)
		if err != nil {
			return "", err
		}
		return string(marshal), nil
	default:
		var sb = new(bytes.Buffer)
		w := printers.GetNewTabWriter(sb)
		genConnectMsg(w, status.List)
		genProxyMsg(w, status.List)
		genCloneMsg(w, status.List)
		_ = w.Flush()
		return sb.String(), nil
	}
}

func genConnectMsg(w *tabwriter.Writer, status []*rpc.Status) {
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", "ID", "Mode", "Cluster", "Kubeconfig", "Namespace", "Status", "Netif")
	for _, c := range status {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.Mode, c.Cluster, c.Kubeconfig, c.Namespace, c.Status, c.Netif)
	}
}

func genProxyMsg(w *tabwriter.Writer, list []*rpc.Status) {
	var needsPrint bool
	for _, status := range list {
		if len(status.ProxyList) != 0 {
			needsPrint = true
			break
		}
	}
	if !needsPrint {
		return
	}

	_, _ = fmt.Fprintf(w, "\n")
	w.SetRememberedWidths(nil)
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", "ID", "Namespace", "Name", "Headers", "IP", "PortMap", "CurrentPC")
	for _, c := range list {
		for _, proxy := range c.ProxyList {
			for _, rule := range proxy.RuleList {
				var headers []string
				for k, v := range rule.Headers {
					headers = append(headers, fmt.Sprintf("%s=%s", k, v))
				}
				if len(headers) == 0 {
					headers = []string{"*"}
				}
				var portmap []string
				for k, v := range rule.PortMap {
					portmap = append(portmap, fmt.Sprintf("%d->%d", k, v))
				}
				_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%v\n",
					c.ID,
					proxy.Namespace,
					proxy.Workload,
					strings.Join(headers, ","),
					rule.LocalTunIPv4,
					strings.Join(portmap, ","),
					rule.CurrentDevice,
				)
			}
		}
	}
}

func genCloneMsg(w *tabwriter.Writer, list []*rpc.Status) {
	var needsPrint bool
	for _, status := range list {
		if len(status.CloneList) != 0 {
			needsPrint = true
			break
		}
	}
	if !needsPrint {
		return
	}

	_, _ = fmt.Fprintf(w, "\n")
	w.SetRememberedWidths(nil)
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", "ID", "Namespace", "Name", "Headers", "ToName", "ToKubeconfig", "ToNamespace", "SyncthingGUI")
	for _, c := range list {
		for _, clone := range c.CloneList {
			//_, _ = fmt.Fprintf(w, "%s\n", clone.Workload)
			for _, rule := range clone.RuleList {
				var headers []string
				for k, v := range rule.Headers {
					headers = append(headers, fmt.Sprintf("%s=%s", k, v))
				}
				if len(headers) == 0 {
					headers = []string{"*"}
				}
				_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					c.ID,
					clone.Namespace,
					clone.Workload,
					strings.Join(headers, ","),
					rule.DstWorkload,
					rule.DstKubeconfig,
					rule.DstNamespace,
					clone.SyncthingGUIAddr,
				)
			}
		}
	}
}

func GetClusterIDByConfig(cmd *cobra.Command, config Config) (string, error) {
	flags := flag.NewFlagSet("", flag.ContinueOnError)
	var sshConf = &pkgssh.SshConfig{}
	pkgssh.AddSshFlags(flags, sshConf)
	handler.AddExtraRoute(flags, &handler.ExtraRouteInfo{})
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

	err := flags.ParseAll(config.Flags, func(flag *flag.Flag, value string) error {
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
	path, err = pkgssh.SshJump(cmd.Context(), sshConf, flags, false)
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
