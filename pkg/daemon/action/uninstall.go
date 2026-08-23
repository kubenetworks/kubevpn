package action

import (
	"context"

	"k8s.io/client-go/kubernetes"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Uninstall handles the Uninstall RPC, removing all KubeVPN server-side resources from the specified namespace.
func (svr *Server) Uninstall(resp rpc.Daemon_UninstallServer) (err error) {
	req, err := resp.Recv()
	if err != nil {
		return err
	}
	_, ctx := svr.initStreamLogger(resp, req.Level, func(msg string) error {
		return resp.Send(&rpc.UninstallResponse{Message: msg})
	})
	kubeconfigBytes, err := resolveKubeconfigBytes(ctx, req.SshJump, req.KubeconfigBytes, false)
	if err != nil {
		return err
	}
	factory := util.InitFactoryByBytes(kubeconfigBytes, req.Namespace)
	clientset, err := factory.KubernetesClientSet()
	if err != nil {
		return err
	}
	return Uninstall(ctx, clientset, req.Namespace)
}

// Uninstall
// 1) quit daemon
// 2) get all proxy-resources from configmap
// 3) cleanup all containers
// 4) cleanup hosts
func Uninstall(ctx context.Context, clientset kubernetes.Interface, ns string) error {
	plog.StepStart(ctx, "Uninstalling traffic manager")
	// Remove the full set of traffic-manager resources KubeVPN creates in this namespace:
	// the eight core resources named config.ConfigMapPodTrafficManager plus the -route and
	// -proxy RBAC (and the cluster-scoped proxy ClusterRole/Binding in central mode).
	// Reuses handler.CleanupTrafficManagerResources so the daemon-layer Uninstall and the
	// handler-layer connect cleanup cannot drift on which resources an uninstall removes.
	handler.CleanupTrafficManagerResources(ctx, clientset, ns)
	_ = cleanupLocalContainer(ctx)
	plog.StepDone(ctx, "Uninstalled traffic manager from namespace %q", ns)
	return nil
}

func cleanupLocalContainer(ctx context.Context) error {
	inspect, err := util.NetworkInspect(ctx, config.ConfigMapPodTrafficManager)
	if err != nil {
		return err
	}
	if len(inspect.Containers) == 0 {
		err = util.NetworkRemove(ctx, config.ConfigMapPodTrafficManager)
	}
	return err
}
