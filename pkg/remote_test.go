package pkg

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/util"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"net"
	"testing"
	"time"
)

//func TestCreateServer(t *testing.T) {
//	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
//		&clientcmd.ClientConfigLoadingRules{ExplicitPath: clientcmd.RecommendedHomeFile}, nil,
//	)
//	config, err := clientConfig.ClientConfig()
//	if err != nil {
//		log.Fatal(err)
//	}
//	clientset, err := kubernetes.NewForConfig(config)
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	i := &net.IPNet{
//		IP:   net.ParseIP("192.168.254.100"),
//		Mask: net.IPv4Mask(255, 255, 255, 0),
//	}
//
//	j := &net.IPNet{
//		IP:   net.ParseIP("172.20.0.0"),
//		Mask: net.IPv4Mask(255, 255, 0, 0),
//	}
//
//	server, err := pkg.CreateOutboundRouterPod(clientset, "test", i, []*net.IPNet{j})
//	fmt.Println(server)
//}

func TestGetIp(t *testing.T) {
	ip := &net.IPNet{
		IP:   net.IPv4(192, 168, 254, 100),
		Mask: net.IPv4Mask(255, 255, 255, 0),
	}
	fmt.Println(ip.String())
}

func TestGetIPFromDHCP(t *testing.T) {
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: clientcmd.RecommendedHomeFile}, nil,
	)
	config, err := clientConfig.ClientConfig()
	if err != nil {
		log.Fatal(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	manager := NewDHCPManager(clientset, "test", nil)
	for i := 0; i < 10; i++ {
		ipNet, err := manager.RentIPRandom()
		ipNet2, err := manager.RentIPRandom()
		if err != nil {
			fmt.Println(err)
			continue
		} else {
			fmt.Printf("%s->%s\n", ipNet.String(), ipNet2.String())
		}
		time.Sleep(time.Millisecond * 10)
		err = manager.ReleaseIpToDHCP(ipNet)
		err = manager.ReleaseIpToDHCP(ipNet2)
		if err != nil {
			fmt.Println(err)
		}
		time.Sleep(time.Millisecond * 10)
	}
}

func TestGetTopController(t *testing.T) {
	s := "/Users/naison/.kube/devpool"
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.KubeConfig = &s
	factory := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))
	controller, err := util.GetTopOwnerReference(factory, "nh90bwck", "pods/services-authors-shadow")
	fmt.Println(controller.Name)
	fmt.Println(controller.Mapping.Resource.Resource)
	fmt.Println(err)
}

func init() {
	util.InitLogger(util.Debug)
}
