package handler

import (
	"context"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// K8sClient bundles the Kubernetes client objects that are shared by
// ConnectOptions and SyncOptions. Embed it to avoid duplicating these
// fields and the InitClient / GetFactory / GetClientset methods.
type K8sClient struct {
	clientset  kubernetes.Interface
	restclient *rest.RESTClient
	config     *rest.Config
	factory    cmdutil.Factory
}

// InitClient initializes the Kubernetes clientset, REST client, and config
// from the given factory. The returned namespace is intentionally not stored
// here because ConnectOptions and SyncOptions keep it in different fields
// (ManagerNamespace vs Namespace). Callers must capture the namespace from
// the second return value.
func (k *K8sClient) InitClient(f cmdutil.Factory) (ns string, err error) {
	plog.G(context.Background()).Debug("Initializing Kubernetes client")
	k.factory = f
	k.config, k.restclient, k.clientset, ns, err = util.InitKubeClient(f)
	return
}

// GetFactory returns the kubectl factory.
func (k *K8sClient) GetFactory() cmdutil.Factory { return k.factory }

// GetClientset returns the Kubernetes clientset.
func (k *K8sClient) GetClientset() kubernetes.Interface { return k.clientset }
