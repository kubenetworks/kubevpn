package util

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/cmd/util"
	"sigs.k8s.io/yaml"

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

func TestWaitBackoff(t *testing.T) {
	var last = time.Now()
	_ = retry.OnError(
		wait.Backoff{
			Steps:    10,
			Duration: time.Millisecond * 50,
		}, func(err error) bool {
			return err != nil
		}, func() error {
			now := time.Now()
			fmt.Println(now.Sub(last).String())
			last = now
			return fmt.Errorf("")
		})
}

func TestArray(t *testing.T) {
	s := []int{1, 2, 3, 1, 2, 3, 1, 2, 3}
	for i := 0; i < 3; i++ {
		ints := s[i*3 : i*3+3]
		println(ints[0], ints[1], ints[2])
	}
}

func TestPatch(t *testing.T) {
	var p = v1.Probe{
		ProbeHandler: v1.ProbeHandler{HTTPGet: &v1.HTTPGetAction{
			Path:   "/health",
			Port:   intstr.FromInt32(9080),
			Scheme: "HTTP",
		}},
	}
	marshal, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(marshal))

	var pp v1.Probe
	err = json.Unmarshal(marshal, &pp)
	if err != nil {
		panic(err)
	}
	bytes, err := yaml.Marshal(pp)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(bytes))
}
