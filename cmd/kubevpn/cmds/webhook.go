package cmds

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
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
			go util.StartupPProfForServer(0)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ns := os.Getenv(config.EnvPodNamespace)
			if ns == "" {
				return fmt.Errorf("failed to get pod namespace")
			}
			clientset, err := f.KubernetesClientSet()
			if err != nil {
				return err
			}
			manager := dhcp.NewDHCPManager(clientset.CoreV1().ConfigMaps(ns), ns)
			return webhook.Main(manager, clientset)
		},
	}
	return cmd
}
