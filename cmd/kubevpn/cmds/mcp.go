package cmds

import (
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	pkgmcp "github.com/wencaiwulue/kubevpn/v2/pkg/mcp"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdMCP(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: i18n.T("Start the MCP (Model Context Protocol) stdio server for AI coding agents"),
		Long: templates.LongDesc(i18n.T(`Start a Model Context Protocol server over stdio exposing KubeVPN's session
workflow (status, connect, proxy, leave, disconnect, logs) to AI coding agents.
Destructive operations (uninstall, reset, quit) are intentionally not exposed.
See https://kubevpn.dev for the agent usage guide.`)),
		RunE: func(cmd *cobra.Command, args []string) error {
			bytes, ns, err := util.ConvertToKubeConfigBytes(f)
			if err != nil {
				return err
			}
			opts := pkgmcp.Options{
				KubeconfigBytes:      bytes,
				Namespace:            ns,
				OriginKubeconfigPath: util.GetKubeConfigPath(f),
				Version:              config.Version,
			}
			return pkgmcp.Run(cmd.Context(), opts)
		},
	}
	return cmd
}
