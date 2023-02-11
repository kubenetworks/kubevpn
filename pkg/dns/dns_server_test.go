package dns

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	miekgdns "github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testclient "k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func TestSetupDnsServer(t *testing.T) {
	port := util.GetAvailableUDPPortOrDie()
	clientConfig := &miekgdns.ClientConfig{
		Servers: []string{"10.233.93.190"},
		Search:  []string{"vke-system.svc.cluster.local", "svc.cluster.local", "cluster.local"},
		Port:    "53",
		Ndots:   0,
	}
	go func() { log.Fatal(NewDNSServer("udp", "127.0.0.1:"+strconv.Itoa(port), clientConfig)) }()
	config := miekgdns.ClientConfig{
		Servers: []string{"127.0.0.1"},
		Search:  clientConfig.Search,
		Port:    strconv.Itoa(port),
		Ndots:   clientConfig.Ndots,
		Timeout: 1,
	}
	_ = os.RemoveAll(filepath.Join("/", "etc", "resolver"))
	if err := os.MkdirAll(filepath.Join("/", "etc", "resolver"), fs.ModePerm); err != nil {
		panic(err)
	}
	for _, s := range strings.Split(clientConfig.Search[0], ".") {
		filename := filepath.Join("/", "etc", "resolver", s)
		err := os.WriteFile(filename, []byte(toString(config)), 0644)
		if err != nil {
			panic(err)
		}
	}
	fmt.Println(port)
	select {}
}

func TestFull(t *testing.T) {
	type Question struct {
		Q string
	}
	type person struct {
		Name     string
		age      *int
		Question []Question
	}

	age := 22
	p := &person{"Bob", &age, []Question{{"haha"}}}
	fmt.Println(p)

	p2 := new(person)
	*p2 = *p
	fmt.Println(p2)
	p.Name = " zhangsan"
	p.Question = append(p.Question, Question{"asdf"})
	fmt.Println(p.Question)

	fmt.Println(p2.Question)
}

func TestWriteHost(t *testing.T) {
	clientset := testclient.NewSimpleClientset()
	go AddServiceNameToHosts(context.Background(), clientset.CoreV1().Services("kube-system"))
	time.AfterFunc(time.Second*2, func() {
		for _, service := range getTestService() {
			_, err := clientset.CoreV1().Services(service.Namespace).Create(context.Background(), &service, metav1.CreateOptions{})
			assert.Nil(t, err)
		}
	})
	select {}
	CancelDNS()
}

func getTestService() (result []v1.Service) {
	return []v1.Service{
		{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Service",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:              "authors",
				Namespace:         "default",
				CreationTimestamp: metav1.Now(),
			},
			Spec: v1.ServiceSpec{
				ClusterIP:    "10.96.164.5",
				ClusterIPs:   []string{"10.96.164.5"},
				ExternalIPs:  nil,
				ExternalName: "",
			},
		},
		{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Service",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:              "ratings",
				Namespace:         "default",
				CreationTimestamp: metav1.Now(),
			},
			Spec: v1.ServiceSpec{
				ClusterIP:    "10.97.28.204",
				ClusterIPs:   []string{"10.97.28.204"},
				ExternalIPs:  nil,
				ExternalName: "",
			},
		},
		{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Service",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:              "details",
				Namespace:         "default",
				CreationTimestamp: metav1.Now(),
			},
			Spec: v1.ServiceSpec{
				ClusterIP:    "10.96.164.5",
				ClusterIPs:   []string{"10.96.164.5"},
				ExternalIPs:  nil,
				ExternalName: "",
			},
		},
		{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Service",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:              "productpage",
				Namespace:         "kube-system",
				CreationTimestamp: metav1.Now(),
			},
			Spec: v1.ServiceSpec{
				ClusterIP:    "10.97.21.170",
				ClusterIPs:   []string{"10.97.21.170"},
				ExternalIPs:  nil,
				ExternalName: "productpage.io",
			},
		},
	}
}

func TestRemoveWrittenHost(t *testing.T) {
	err := updateHosts("")
	assert.Nil(t, err)
}

func TestFix(t *testing.T) {
	clientConfig := &miekgdns.ClientConfig{
		Servers: []string{"10.233.93.190"},
		Search:  []string{"vke-system.svc.cluster.local", "svc.cluster.local", "cluster.local"},
		Port:    "53",
		Ndots:   5,
	}
	for _, s := range clientConfig.NameList("productpage") {
		println(s)
	}
}
