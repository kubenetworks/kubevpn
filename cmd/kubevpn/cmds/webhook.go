package cmds

import (
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/webhook"
)

func CmdWebhook(cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhook",
		Short: "Starts a HTTP server, useful for creating MutatingAdmissionWebhook",
		Long: `Starts a HTTP server, useful for creating MutatingAdmissionWebhook.
After deploying it to Kubernetes cluster, the Administrator needs to create a MutatingWebhookConfiguration
in the Kubernetes cluster to register remote webhook admission controllers.`,
		Args: cobra.MaximumNArgs(0),
		Run:  webhook.Main,
	}
	return cmd
}
