package action

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

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
	options := metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}
	// The eight namespaced traffic-manager resources, named config.ConfigMapPodTrafficManager.
	// Shares the deletion list with handler.cleanupTrafficManagerResources so the two paths
	// cannot drift on which resources an uninstall must remove.
	handler.DeleteTrafficManagerCoreResources(ctx, clientset, ns, config.ConfigMapPodTrafficManager, options)
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
