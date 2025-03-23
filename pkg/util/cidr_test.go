package util

import (
	"context"
	"fmt"
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/cmd/util"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

var (
	namespace  string
	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	restconfig *rest.Config
	f          util.Factory
)

func before() {
	var err error
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	f = util.NewFactory(util.NewMatchVersionFlags(configFlags))

	if restconfig, err = f.ToRESTConfig(); err != nil {
		plog.G(context.Background()).Fatal(err)
	}
	if restclient, err = rest.RESTClientFor(restconfig); err != nil {
		plog.G(context.Background()).Fatal(err)
	}
	if clientset, err = kubernetes.NewForConfig(restconfig); err != nil {
		plog.G(context.Background()).Fatal(err)
	}
	if namespace, _, err = f.ToRawKubeConfigLoader().Namespace(); err != nil {
		plog.G(context.Background()).Fatal(err)
	}
}

func TestByDumpClusterInfo(t *testing.T) {
	before()
	info, err := GetCIDRByDumpClusterInfo(context.Background(), clientset)
	if err != nil {
		t.Error(err)
	}
	for _, ipNet := range info {
		fmt.Println(ipNet.String())
	}
}

func TestByCreateSvc(t *testing.T) {
	before()
	info, err := GetServiceCIDRByCreateService(context.Background(), clientset.CoreV1().Services("default"))
	if err != nil {
		t.Error(err)
	}
	fmt.Println(info)
}

func TestElegant(t *testing.T) {
	before()
	elegant, err := GetCIDRElegant(context.Background(), clientset, restconfig, namespace)
	if err != nil {
		t.Error(err)
	}
	for _, net := range elegant {
		fmt.Println(net.String())
	}
}
