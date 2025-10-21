package cmds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	FormatJson      = "json"
	FormatYaml      = "yaml"
	FormatTable     = "table"
	FormatTableWide = "wide"
)

func CmdStatus(f cmdutil.Factory) *cobra.Command {
	var aliasName string
	var localFile string
	var remoteAddr string
	var format string
	cmd := &cobra.Command{
		Use:   "status",
		Short: i18n.T("Show connect status and list proxy/sync resource"),
		Long: templates.LongDesc(i18n.T(`
		Show connect status and list proxy/sync resource

		Show connect status and list proxy or sync resource, you can check connect status by filed status and netif.
		if netif is empty, means tun device closed, so it's unhealthy, it will also show route info, if proxy workloads, 
		not only show myself proxy resource, another route info will also display.
		`)),
		Example: templates.Examples(i18n.T(`
        # show status for connect status and list proxy/sync resource 
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
			var connectionIDs []string
			if aliasName != "" {
				configs, err := ParseAndGet(localFile, remoteAddr, aliasName)
				if err != nil {
					return err
				}
				for _, conf := range configs {
					connectionID, err := GetConnectionIDByConfig(cmd, conf)
					if err != nil {
						return err
					}
					connectionIDs = append(connectionIDs, connectionID)
				}
			}

			cli, err := daemon.GetClient(false)
			if err != nil {
				return err
			}
			resp, err := cli.Status(cmd.Context(), &rpc.StatusRequest{ConnectionIDs: connectionIDs})
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
	cmd.Flags().StringVarP(&localFile, "kubevpnconfig", "f", util.If(os.Getenv("KUBEVPNCONFIG") != "", os.Getenv("KUBEVPNCONFIG"), config.GetConfigFile()), "Path to the kubevpnconfig file to use for CLI requests.")
	cmd.Flags().StringVarP(&remoteAddr, "remote", "r", "", "Remote config file, eg: https://raw.githubusercontent.com/kubenetworks/kubevpn/master/pkg/config/config.yaml")
	cmd.Flags().StringVarP(&format, "output", "o", FormatTable, fmt.Sprintf("Output format. One of: (%s, %s, %s, %s)", FormatJson, FormatYaml, FormatTable, FormatTableWide))
	return cmd
}

func genOutput(status *rpc.StatusResponse, format string) (string, error) {
	switch format {
	case FormatJson:
		if len(status.List) == 0 {
			return "", nil
		}
		marshal, err := json.Marshal(status)
		if err != nil {
			return "", err
		}
		return string(marshal), nil

	case FormatYaml:
		if len(status.List) == 0 {
			return "", nil
		}
		marshal, err := yaml.Marshal(status)
		if err != nil {
			return "", err
		}
		return string(marshal), nil
	case FormatTableWide:
		var sb = new(bytes.Buffer)
		w := printers.GetNewTabWriter(sb)
		genConnectWideMsg(w, status.CurrentConnectionID, status.List)
		genProxyMsg(w, status.List)
		genSyncMsg(w, status.List)
		_ = w.Flush()
		return sb.String(), nil
	default:
		var sb = new(bytes.Buffer)
		w := printers.GetNewTabWriter(sb)
		genConnectMsg(w, status.CurrentConnectionID, status.List)
		genProxyMsg(w, status.List)
		genSyncMsg(w, status.List)
		_ = w.Flush()
		return sb.String(), nil
	}
}

func genConnectMsg(w *tabwriter.Writer, currentConnectionID string, status []*rpc.Status) {
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", "CURRENT", "CONNECTION ID", "CLUSTER", "KUBECONFIG", "NAMESPACE", "STATUS", "NETIF")
	for _, c := range status {
		current := util.If[string](c.ConnectionID == currentConnectionID, "*", "")
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", current, c.ConnectionID, c.Cluster, shortenPath(c.Kubeconfig), c.Namespace, c.Status, c.Netif)
	}
}

func genConnectWideMsg(w *tabwriter.Writer, currentConnectionID string, status []*rpc.Status) {
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", "CURRENT", "CONNECTION ID", "CLUSTER", "KUBECONFIG", "NAMESPACE", "STATUS", "NETIF", "IP")
	for _, c := range status {
		current := util.If[string](c.ConnectionID == currentConnectionID, "*", "")
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", current, c.ConnectionID, c.Cluster, shortenPath(c.Kubeconfig), c.Namespace, c.Status, c.Netif, c.IPv4)
	}
}

func shortenPath(absPath string) string {
	// on windows
	// cmd.exe not recognize '~', eg: cd ~, will error
	// powershell.exe can recognize '~'
	if util.IsWindows() {
		return absPath
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return absPath
	}
	relativePath, err := filepath.Rel(homeDir, absPath)
	if err != nil {
		return absPath
	}
	if strings.HasPrefix(relativePath, "..") {
		return absPath
	}
	return filepath.Join("~", relativePath)
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
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", strings.Repeat(" ", len("CURRENT")), "CONNECTION ID", "NAMESPACE", "NAME", "HEADERS", "PORTS", "CURRENT PC")
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
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%v\n",
					"",
					c.ConnectionID,
					proxy.Namespace,
					proxy.Workload,
					strings.Join(headers, ","),
					strings.Join(portmap, ","),
					rule.CurrentDevice,
				)
			}
		}
	}
}

func genSyncMsg(w *tabwriter.Writer, list []*rpc.Status) {
	var needsPrint bool
	for _, status := range list {
		if len(status.SyncList) != 0 {
			needsPrint = true
			break
		}
	}
	if !needsPrint {
		return
	}

	_, _ = fmt.Fprintf(w, "\n")
	w.SetRememberedWidths(nil)
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", strings.Repeat(" ", len("CURRENT")), "CONNECTION ID", "NAMESPACE", "NAME", "HEADERS", "TO NAME", "SYNCTHING GUI")
	for _, c := range list {
		for _, sync := range c.SyncList {
			//_, _ = fmt.Fprintf(w, "%s\n", sync.Workload)
			for _, rule := range sync.RuleList {
				var headers []string
				for k, v := range rule.Headers {
					headers = append(headers, fmt.Sprintf("%s=%s", k, v))
				}
				if len(headers) == 0 {
					headers = []string{"*"}
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					"",
					c.ConnectionID,
					sync.Namespace,
					sync.Workload,
					strings.Join(headers, ","),
					rule.DstWorkload,
					sync.SyncthingGUIAddr,
				)
			}
		}
	}
}

func GetConnectionIDByConfig(cmd *cobra.Command, config Config) (string, error) {
	flags := flag.NewFlagSet("", flag.ContinueOnError)
	var sshConf = &pkgssh.SshConfig{}
	pkgssh.AddSshFlags(flags, sshConf)
	handler.AddExtraRoute(flags, &handler.ExtraRouteInfo{})
	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.AddFlags(flags)
	var kubeconfigJson string
	AddKubeconfigJsonFlag(flags, &kubeconfigJson)
	matchVersionFlags := cmdutil.NewMatchVersionFlags(&warp{ConfigFlags: configFlags, KubeconfigJson: kubeconfigJson})
	matchVersionFlags.AddFlags(flags)
	f := cmdutil.NewFactory(matchVersionFlags)

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
	kubeConfigBytes, ns, err := util.ConvertToKubeConfigBytes(f)
	if err != nil {
		return "", err
	}
	var file string
	defer os.Remove(file)
	if !sshConf.IsEmpty() {
		file, err = pkgssh.SshJump(cmd.Context(), sshConf, kubeConfigBytes, false)
	} else {
		file, err = util.ConvertToTempKubeconfigFile(kubeConfigBytes, "")
	}
	if err != nil {
		return "", err
	}
	var c = &handler.ConnectOptions{}
	err = c.InitClient(util.InitFactoryByPath(file, ns))
	if err != nil {
		return "", err
	}
	id, err := util.GetConnectionID(cmd.Context(), c.GetClientset().CoreV1().Namespaces(), ns)
	if err != nil {
		return "", err
	}
	return id, nil
}

func ParseArgs(cmd *cobra.Command, conf *Config) error {
	var str string
	for i := 0; i < len(conf.Flags); i++ {
		kubeconfigJson, err := parseKubeconfigJson(cmd, []string{conf.Flags[i]})
		if err != nil {
			return err
		}
		if kubeconfigJson != "" {
			str = kubeconfigJson
			conf.Flags = append(conf.Flags[:i], conf.Flags[i+1:]...)
			i--
		}
	}

	if str == "" {
		return nil
	}

	file, err := util.ConvertToTempKubeconfigFile([]byte(str), filepath.Join(config.GetTempPath(), conf.Name))
	if err != nil {
		return err
	}
	conf.Flags = append(conf.Flags, fmt.Sprintf("%s=%s", "--kubeconfig", file))
	return nil
}

func parseKubeconfigJson(cmd *cobra.Command, args []string) (string, error) {
	flags := flag.NewFlagSet("", flag.ContinueOnError)
	var sshConf = &pkgssh.SshConfig{}
	pkgssh.AddSshFlags(flags, sshConf)
	handler.AddExtraRoute(flags, &handler.ExtraRouteInfo{})
	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.AddFlags(flags)
	var kubeconfigJson string
	AddKubeconfigJsonFlag(flags, &kubeconfigJson)
	matchVersionFlags := cmdutil.NewMatchVersionFlags(&warp{ConfigFlags: configFlags})
	matchVersionFlags.AddFlags(flags)

	for _, command := range cmd.Parent().Commands() {
		command.Flags().VisitAll(func(f *flag.Flag) {
			if flags.Lookup(f.Name) == nil && flags.ShorthandLookup(f.Shorthand) == nil {
				flags.AddFlag(f)
			}
		})
	}

	err := flags.ParseAll(args, func(flag *flag.Flag, value string) error {
		_ = flags.Set(flag.Name, value)
		return nil
	})
	return kubeconfigJson, err
}
