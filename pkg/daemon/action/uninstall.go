package action

import (
	"context"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
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
	file, err := resolveKubeconfig(ctx, req.SshJump, req.KubeconfigBytes, false)
	if err != nil {
		return err
	}
	defer os.Remove(file)
	factory := util.InitFactoryByPath(file, req.Namespace)
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
	name := config.ConfigMapPodTrafficManager
	options := metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}
	_ = clientset.CoreV1().ConfigMaps(ns).Delete(ctx, name, options)
	_ = clientset.CoreV1().Pods(ns).Delete(ctx, config.CniNetName, options)
	_ = clientset.CoreV1().Secrets(ns).Delete(ctx, name, options)
	_ = clientset.RbacV1().RoleBindings(ns).Delete(ctx, name, options)
	_ = clientset.CoreV1().ServiceAccounts(ns).Delete(ctx, name, options)
	_ = clientset.RbacV1().Roles(ns).Delete(ctx, name, options)
	_ = clientset.CoreV1().Services(ns).Delete(ctx, name, options)
	_ = clientset.AppsV1().Deployments(ns).Delete(ctx, name, options)
	_ = clientset.BatchV1().Jobs(ns).Delete(ctx, name, options)
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
