package cmds

import (
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	"github.com/wencaiwulue/kubevpn/v2/pkg/webhook"
)

func CmdWebhook(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "webhook",
		Hidden: true,
		Short:  i18n.T("Starts a HTTP server, useful for creating MutatingAdmissionWebhook"),
		Long: templates.LongDesc(i18n.T(`
		Starts a HTTP server, useful for creating MutatingAdmissionWebhook.
		After deploying it to Kubernetes cluster, the Administrator needs to create a MutatingWebhookConfiguration
		in the Kubernetes cluster to register remote webhook admission controllers.
		`)),
		Args: cobra.MaximumNArgs(0),
		PreRun: func(cmd *cobra.Command, args []string) {
			util.InitLoggerForServer(true)
			go util.StartupPProfForServer(0)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return webhook.Main(f)
		},
	}
	return cmd
}
