package cmds

import (
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/util"
	"github.com/wencaiwulue/kubevpn/pkg/webhook"
)

func CmdWebhook(f cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "webhook",
		Hidden: true,
		Short:  "Starts a HTTP server, useful for creating MutatingAdmissionWebhook",
		Long: `Starts a HTTP server, useful for creating MutatingAdmissionWebhook.
After deploying it to Kubernetes cluster, the Administrator needs to create a MutatingWebhookConfiguration
in the Kubernetes cluster to register remote webhook admission controllers.`,
		Args: cobra.MaximumNArgs(0),
		PreRun: func(cmd *cobra.Command, args []string) {
			util.InitLogger(true)
			go util.StartupPProf(0)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return webhook.Main(f)
		},
	}
	return cmd
}
