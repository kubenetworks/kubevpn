package cmds

import (
	"github.com/spf13/cobra"

	"github.com/wencaiwulue/kubevpn/pkg/webhook"
)

var CmdWebhook = &cobra.Command{
	Use:   "webhook",
	Short: "Starts a HTTP server, useful for creating MutatingAdmissionWebhook and ValidatingAdmissionWebhook",
	Long: `Starts a HTTP server, useful for creating MutatingAdmissionWebhook and ValidatingAdmissionWebhook.
After deploying it to Kubernetes cluster, the Administrator needs to create a ValidatingWebhookConfiguration
in the Kubernetes cluster to register remote webhook admission controllers.`,
	Args: cobra.MaximumNArgs(0),
	Run:  webhook.Main,
}

func init() {
	RootCmd.AddCommand(CmdWebhook)
}
