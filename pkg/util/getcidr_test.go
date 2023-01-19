package util

import (
	"encoding/base64"
	"fmt"
	"testing"

	log "github.com/sirupsen/logrus"
	"golang.org/x/text/encoding/simplifiedchinese"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/pointer"
)

func before() {
	var err error
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.KubeConfig = pointer.String("/Users/bytedance/.kube/vestack_upgrade")
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
		panic(err)
	}
	for _, ipNet := range info {
		fmt.Println(ipNet.String())
	}
}

func TestByCreateSvc(t *testing.T) {
	before()
	info, err := getServiceCIDRByCreateSvc(clientset.CoreV1().Services("default"))
	if err != nil {
		panic(err)
	}
	fmt.Println(info)
}

func TestElegant(t *testing.T) {
	before()
	elegant, err := GetCIDRElegant(clientset, restclient, restconfig, namespace)
	if err != nil {
		panic(err)
	}
	for _, net := range elegant {
		fmt.Println(net.String())
	}
}

func TestCal(t *testing.T) {
	ints := []int{
		5, 26,
		8, 22,
		25, 8,
		8, 10,
		25, 23,
		8, 22,
		8, 24,
		8, 23,
	}
	sum := 0
	for _, i := range ints {
		sum += i
	}
	println(sum)
}

func TestName222(t *testing.T) {
	ss := `DQrDu9PQ0+vWuLaoserXvM/gxqXF5LXEuebU8qGjDQoNCg==`
	out, err := base64.StdEncoding.DecodeString(ss)
	if err != nil {
		panic(err)
	}
	s := string(out)
	var b []byte
	b, err = simplifiedchinese.GB18030.NewDecoder().Bytes(out)
	if err == nil {
		s = string(b)
		println(s)
	}
	b, err = simplifiedchinese.GBK.NewDecoder().Bytes(out)
	if err == nil {
		s = string(b)
		println(s)
	}
	b, err = simplifiedchinese.HZGB2312.NewDecoder().Bytes(out)
	if err == nil {
		s = string(b)
	}
	println(string(s))
}
