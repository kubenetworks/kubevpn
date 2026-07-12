//go:build integration

// These tests talk to a real Kubernetes cluster (they read CIDRs by dumping cluster
// info / creating a probe Service), so they require a working kubeconfig. They live
// behind the `integration` build tag and are excluded from the no-cluster suite that
// runs on Windows. See the `ut` vs `ut-no-cluster` Makefile targets.
package util

import (
	"context"
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/cmd/util"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

type cidrUt struct {
	namespace  string
	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	restconfig *rest.Config
	f          util.Factory
}

func (u *cidrUt) init() {
	var err error
	configFlags := genericclioptions.NewConfigFlags(true)
	u.f = util.NewFactory(util.NewMatchVersionFlags(configFlags))

	if u.restconfig, err = u.f.ToRESTConfig(); err != nil {
		plog.G(context.Background()).Fatal(err)
	}
	if u.restclient, err = rest.RESTClientFor(u.restconfig); err != nil {
		plog.G(context.Background()).Fatal(err)
	}
	if u.clientset, err = kubernetes.NewForConfig(u.restconfig); err != nil {
		plog.G(context.Background()).Fatal(err)
	}
	if u.namespace, _, err = u.f.ToRawKubeConfigLoader().Namespace(); err != nil {
		plog.G(context.Background()).Fatal(err)
	}
}

func TestByDumpClusterInfo(t *testing.T) {
	u := &cidrUt{}
	u.init()
	info, err := GetCIDRByDumpClusterInfo(context.Background(), u.clientset)
	if err != nil {
		t.Log(err.Error())
	}
	for _, ipNet := range info {
		t.Log(ipNet.String())
	}
}

func TestByCreateSvc(t *testing.T) {
	u := &cidrUt{}
	u.init()
	info, err := GetServiceCIDRByCreateService(context.Background(), u.clientset.CoreV1().Services("default"))
	if err != nil {
		t.Log(err.Error())
	}
	if info != nil {
		t.Log(info.String())
	}
}

func TestElegant(t *testing.T) {
	u := &cidrUt{}
	u.init()
	elegant := GetCIDR(context.Background(), u.clientset, u.namespace)
	for _, ipNet := range elegant {
		t.Log(ipNet.String())
	}
}
