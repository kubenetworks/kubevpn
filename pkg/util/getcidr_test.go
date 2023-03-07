package util

import (
	"fmt"
	"testing"

	log "github.com/sirupsen/logrus"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/cmd/util"
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
		log.Fatal(err)
	}
	if restclient, err = rest.RESTClientFor(restconfig); err != nil {
		log.Fatal(err)
	}
	if clientset, err = kubernetes.NewForConfig(restconfig); err != nil {
		log.Fatal(err)
	}
	if namespace, _, err = f.ToRawKubeConfigLoader().Namespace(); err != nil {
		log.Fatal(err)
	}
}

func TestByDumpClusterInfo(t *testing.T) {
	before()
	info, err := getCIDRByDumpClusterInfo(clientset)
	if err != nil {
		t.Error(err)
	}
	for _, ipNet := range info {
		fmt.Println(ipNet.String())
	}
}

func TestByCreateSvc(t *testing.T) {
	before()
	info, err := getServiceCIDRByCreateSvc(clientset.CoreV1().Services("default"))
	if err != nil {
		t.Error(err)
	}
	fmt.Println(info)
}

func TestElegant(t *testing.T) {
	before()
	elegant, err := GetCIDRElegant(clientset, restclient, restconfig, namespace)
	if err != nil {
		t.Error(err)
	}
	for _, net := range elegant {
		fmt.Println(net.String())
	}
}
