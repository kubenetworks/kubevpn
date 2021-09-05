package remote

import (
	"context"
	"encoding/json"
	"fmt"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/apps/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateServer(t *testing.T) {
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{
			ExplicitPath: filepath.Join(homedir.HomeDir(), clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName),
		},
		nil,
	)
	config, err := clientConfig.ClientConfig()
	if err != nil {
		log.Fatal(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	i := &net.IPNet{
		IP:   net.ParseIP("192.168.254.100"),
		Mask: net.IPv4Mask(255, 255, 255, 0),
	}

	j := &net.IPNet{
		IP:   net.ParseIP("172.20.0.0"),
		Mask: net.IPv4Mask(255, 255, 0, 0),
	}

	server, err := CreateServerOutbound(clientset, "test", i, []*net.IPNet{j})
	fmt.Println(server)
}

func TestGetIp(t *testing.T) {
	ip := &net.IPNet{
		IP:   net.IPv4(192, 168, 254, 100),
		Mask: net.IPv4Mask(255, 255, 255, 0),
	}
	fmt.Println(ip.String())
}

func TestGetIPFromDHCP(t *testing.T) {
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{
			ExplicitPath: filepath.Join(homedir.HomeDir(), clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName),
		},
		nil,
	)
	config, err := clientConfig.ClientConfig()
	if err != nil {
		log.Fatal(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	err = InitDHCP(clientset, "test", nil)
	if err != nil {
		fmt.Println(err)
	}
	for i := 0; i < 10; i++ {
		ipNet, err := GetIpFromDHCP(clientset, "test")
		ipNet2, err := GetIpFromDHCP(clientset, "test")
		if err != nil {
			fmt.Println(err)
			continue
		} else {
			fmt.Printf("%s->%s\n", ipNet.String(), ipNet2.String())
		}
		time.Sleep(time.Millisecond * 10)
		err = ReleaseIpToDHCP(clientset, "test", ipNet)
		err = ReleaseIpToDHCP(clientset, "test", ipNet2)
		if err != nil {
			fmt.Println(err)
		}
		time.Sleep(time.Millisecond * 10)
	}
}

func TestOwnerRef(t *testing.T) {
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: clientcmd.RecommendedHomeFile}, nil,
	)
	config, _ := clientConfig.ClientConfig()
	clientset, _ := kubernetes.NewForConfig(config)
	//get, _ := clientset.CoreV1().Pods("test").Get(context.Background(), "tomcat-7449544d95-nv7gr", metav1.GetOptions{})
	get, _ := clientset.CoreV1().Pods("test").Get(context.Background(), "mysql-0", metav1.GetOptions{})

	of := metav1.GetControllerOf(get)
	for of != nil {
		b, err := clientset.AppsV1().RESTClient().Get().Namespace("test").
			Name(of.Name).Resource(strings.ToLower(of.Kind) + "s").Do(context.Background()).Raw()
		if k8serrors.IsNotFound(err) {
			return
		}
		var replicaSet v1.ReplicaSet
		if err = json.Unmarshal(b, &replicaSet); err == nil && len(replicaSet.Name) != 0 {
			fmt.Printf("%s-%s\n", replicaSet.Kind, replicaSet.Name)
			of = metav1.GetControllerOfNoCopy(&replicaSet)
			continue
		}
		var statefulSet v1.StatefulSet
		if err = json.Unmarshal(b, &statefulSet); err == nil && len(statefulSet.Name) != 0 {
			fmt.Printf("%s-%s\n", statefulSet.Kind, statefulSet.Name)
			of = metav1.GetControllerOfNoCopy(&statefulSet)
			continue
		}
		var deployment v1.Deployment
		if err = json.Unmarshal(b, &deployment); err == nil && len(deployment.Name) != 0 {
			fmt.Printf("%s-%s\n", deployment.Kind, deployment.Name)
			of = metav1.GetControllerOfNoCopy(&deployment)
			continue
		}
	}
}
