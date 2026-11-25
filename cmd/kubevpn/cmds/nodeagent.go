package cmds

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/nodeagent"
)

// CmdNodeAgent runs one node-local agent: a single tunnel plus local DNAT and
// Envoy supervision driven by the manager's desired attachments.
func CmdNodeAgent() *cobra.Command {
	return &cobra.Command{
		Use:    "node-agent",
		Short:  "Run the node-boundary traffic agent",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			managerNamespace := os.Getenv("MANAGER_NAMESPACE")
			nodeName := os.Getenv("NODE_NAME")
			podIP := os.Getenv("POD_IP")
			if managerNamespace == "" || nodeName == "" || podIP == "" {
				return fmt.Errorf("MANAGER_NAMESPACE, NODE_NAME and POD_IP are required")
			}
			image := os.Getenv("IMAGE")
			if image == "" {
				image = config.Image
			}
			restConfig, err := rest.InClusterConfig()
			if err != nil {
				return fmt.Errorf("load in-cluster config: %w", err)
			}
			clientset, err := kubernetes.NewForConfig(restConfig)
			if err != nil {
				return fmt.Errorf("create kubernetes client: %w", err)
			}
			return nodeagent.Run(cmd.Context(), nodeagent.Options{
				Clientset:        clientset,
				ManagerNamespace: managerNamespace,
				NodeName:         nodeName,
				PodIP:            podIP,
				Image:            image,
			})
		},
	}
}
