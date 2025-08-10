package action

import (
	"context"
	"io"
	"os"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Uninstall(resp rpc.Daemon_UninstallServer) (err error) {
	req, err := resp.Recv()
	if err != nil {
		return err
	}
	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newUninstallWarp(resp), svr.LogFile))

	var file string
	defer os.Remove(file)
	var sshConf = ssh.ParseSshFromRPC(req.SshJump)
	var ctx = plog.WithLogger(resp.Context(), logger)
	if !sshConf.IsEmpty() {
		file, err = ssh.SshJump(ctx, sshConf, []byte(req.KubeconfigBytes), false)
	} else {
		file, err = util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes), "")
	}
	if err != nil {
		return err
	}
	factory := util.InitFactoryByPath(file, req.Namespace)
	clientset, err := factory.KubernetesClientSet()
	if err != nil {
		return err
	}
	err = Uninstall(ctx, clientset, req.Namespace)
	if err != nil {
		return err
	}
	return nil
}

// Uninstall
// 1) quit daemon
// 2) get all proxy-resources from configmap
// 3) cleanup all containers
// 4) cleanup hosts
func Uninstall(ctx context.Context, clientset *kubernetes.Clientset, ns string) error {
	plog.G(ctx).Infof("Cleaning up resources")
	name := config.ConfigMapPodTrafficManager
	options := metav1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)}
	_ = clientset.CoreV1().ConfigMaps(ns).Delete(ctx, name, options)
	_ = clientset.CoreV1().Pods(ns).Delete(ctx, config.CniNetName, options)
	_ = clientset.CoreV1().Secrets(ns).Delete(ctx, name, options)
	_ = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(ctx, name+"."+ns, options)
	_ = clientset.RbacV1().RoleBindings(ns).Delete(ctx, name, options)
	_ = clientset.CoreV1().ServiceAccounts(ns).Delete(ctx, name, options)
	_ = clientset.RbacV1().Roles(ns).Delete(ctx, name, options)
	_ = clientset.CoreV1().Services(ns).Delete(ctx, name, options)
	_ = clientset.AppsV1().Deployments(ns).Delete(ctx, name, options)
	_ = clientset.BatchV1().Jobs(ns).Delete(ctx, name, options)
	_ = CleanupLocalContainer(ctx)
	plog.G(ctx).Info("Done")
	return nil
}

func CleanupLocalContainer(ctx context.Context) error {
	inspect, err := util.NetworkInspect(ctx, config.ConfigMapPodTrafficManager)
	if err != nil {
		return err
	}
	if len(inspect.Containers) == 0 {
		err = util.NetworkRemove(ctx, config.ConfigMapPodTrafficManager)
	}
	return err
}

type uninstallWarp struct {
	server rpc.Daemon_UninstallServer
}

func (r *uninstallWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.UninstallResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newUninstallWarp(server rpc.Daemon_UninstallServer) io.Writer {
	return &uninstallWarp{server: server}
}
