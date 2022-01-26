package pkg

import (
	"encoding/json"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/util"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	pkgresource "k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"net"
	"os/exec"
	"path/filepath"
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

	manager := NewDHCPManager(clientset, "test")
	manager.InitDHCP()
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

func TestGetTopControllerByLabel(t *testing.T) {
	s := "/Users/naison/.kube/mesh"
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.KubeConfig = &s
	factory := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))
	controller, err := util.GetTopOwnerReferenceBySelector(factory, "default", "app=productpage")
	fmt.Println(controller)
	fmt.Println(err)
}

func TestPreCheck(t *testing.T) {
	options := ConnectOptions{
		KubeconfigPath: filepath.Join(homedir.HomeDir(), ".kube", "mesh"),
		Namespace:      "naison-test",
		Mode:           "reverse",
		Workloads:      []string{"services/authors"},
	}
	options.InitClient()
	options.PreCheckResource()
	fmt.Println(options.Workloads)
}

func init() {
	util.InitLogger(util.Debug)
}

func TestBackoff(t *testing.T) {
	var last = time.Now()
	retry.OnError(wait.Backoff{
		Steps:    10,
		Duration: 40 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.5,
	}, func(err error) bool {
		return true
	}, func() error {
		now := time.Now()
		fmt.Printf("%vs\n", now.Sub(last).Seconds())
		last = now
		return errors.New("")
	})
}

func TestGetCRD(t *testing.T) {
	join := filepath.Join(homedir.HomeDir(), ".kube", "nocalhost.large")
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.KubeConfig = &join
	factory := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))
	Namespace, _, _ := factory.ToRawKubeConfigLoader().Namespace()
	object, err := util.GetUnstructuredObject(factory, Namespace, "statefulsets.apps.kruise.io/sample-beta1")
	fmt.Println(object)
	fmt.Println(err)
}

func TestDeleteAndCreate(t *testing.T) {
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.KubeConfig = &clientcmd.RecommendedHomeFile
	factory := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))

	//config, err := factory.ToRESTConfig()
	//restclient, err := factory.RESTClient()
	//clientset, err := factory.KubernetesClientSet()
	Namespace, _, err := factory.ToRawKubeConfigLoader().Namespace()
	object, err := util.GetUnstructuredObject(factory, Namespace, "statefulsets.apps.kruise.io/sample-beta1")

	u := object.Object.(*unstructured.Unstructured)
	var pp v1.Pod
	marshal, err := json.Marshal(u)
	err = json.Unmarshal(marshal, &pp)

	helper := pkgresource.NewHelper(object.Client, object.Mapping)
	//zero := int64(0)
	if _, err = helper.DeleteWithOptions(object.Namespace, object.Name, &metav1.DeleteOptions{
		//GracePeriodSeconds: &zero,
	}); err != nil {
		log.Fatal(err)
	}

	if single, err := helper.WatchSingle(object.Namespace, object.Name, object.ResourceVersion); err == nil {
	out:
		for {
			select {
			case e, ok := <-single.ResultChan():
				if ok {
					if e.Type == watch.Deleted {
						single.Stop()
						break out
					}
				}
			}
		}
	}

	_ = exec.Command("kubectl", "wait", "pods/nginx", "--for=delete").Run()

	p := &v1.Pod{ObjectMeta: pp.ObjectMeta, Spec: pp.Spec}
	CleanupUselessInfo(p)
	if err = retry.OnError(wait.Backoff{
		Steps:    10,
		Duration: 50 * time.Millisecond,
		Factor:   5.0,
		Jitter:   1,
	}, func(err error) bool {
		return !k8serrors.IsAlreadyExists(err)
	}, func() error {
		//if get, err2 := helper.Get(object.Namespace, object.Name); err2 == nil {
		//	if ppp, ok := get.(*v1.Pod); ok {
		//		if ppp.Status.Phase == v1.PodRunning {
		//			return nil
		//		}
		//	}
		//	return errors.New("")
		//}
		_, err = helper.Create(object.Namespace, true, p)
		if err != nil {
			return err
		}
		return errors.New("")
	}); !k8serrors.IsAlreadyExists(err) {
		log.Fatal(err)
	}
}
