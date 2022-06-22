package util

import (
	"context"
	"fmt"
	"net"
	"testing"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/config"
)

var (
	namespace  string
	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	restconfig *rest.Config
)

func TestShell(t *testing.T) {
	var err error

	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.KubeConfig = &clientcmd.RecommendedHomeFile
	f := util.NewFactory(util.NewMatchVersionFlags(configFlags))

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

	out, err := Shell(clientset, restclient, restconfig, config.PodTrafficManager, namespace, "cat /etc/resolv.conf | grep nameserver | awk '{print$2}'")
	serviceList, err := clientset.CoreV1().Services(v1.NamespaceSystem).List(context.Background(), v1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", "kube-dns").String(),
	})

	fmt.Println(out == serviceList.Items[0].Spec.ClusterIP)
}

func TestDeleteRule(t *testing.T) {
	DeleteWindowsFirewallRule(context.Background())
}

func TestUDP(t *testing.T) {
	relay, err := net.ListenUDP("udp", &net.UDPAddr{Port: 12345})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(relay.LocalAddr())
	fmt.Println(relay.RemoteAddr())
}
