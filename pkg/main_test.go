package main

import (
	"context"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"os/exec"
	"path/filepath"
	"testing"
)

var (
	clientConfig = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{
			ExplicitPath: filepath.Join(homedir.HomeDir(), clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName),
		},
		nil,
	)
	clientconfig, _  = clientConfig.ClientConfig()
	clientsets, _    = kubernetes.NewForConfig(clientconfig)
	namespaces, _, _ = clientConfig.Namespace()
)

func TestCidr(t *testing.T) {
	cidr, err := getCIDR(clientsets, namespaces)
	if err == nil {
		fmt.Println(cidr.String())
	}
}

func TestPing(t *testing.T) {
	list, _ := clientsets.CoreV1().Services(namespaces).List(context.Background(), metav1.ListOptions{})
	for _, service := range list.Items {
		for _, clusterIP := range service.Spec.ClusterIPs {
			_ = exec.Command("ping", clusterIP, "-c", "4").Run()
		}
	}
}
