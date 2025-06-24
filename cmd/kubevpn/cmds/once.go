package cmds

import (
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

func CmdOnce(factory cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "once",
		Short: i18n.T("Once generate TLS and restart deployment"),
		Long:  templates.LongDesc(i18n.T(`Once generate TLS and restart deployment for helm installation.`)),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			return handler.Once(cmd.Context(), factory)
		},
		Hidden:                true,
		DisableFlagsInUseLine: true,
	}
	return cmd
}
