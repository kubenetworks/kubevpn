package nodeagent

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestBuildTunnelCommand_SetsManagerServiceAndAgent(t *testing.T) {
	const managerAddr = "kubevpn-traffic-manager.kubevpn:10801"
	cmd := buildTunnelCommand(context.Background(), "kubevpn", managerAddr, "node-a")
	if args := strings.Join(cmd.Args, " "); !strings.Contains(args, "agent=node-a") {
		t.Fatalf("tunnel URL does not identify the node agent: %s", args)
	}
	wantEnv := "TrafficManagerService=" + managerAddr
	for _, env := range cmd.Env {
		if env == wantEnv {
			return
		}
	}
	t.Fatalf("tunnel environment is missing %s", wantEnv)
}

func TestAttachmentSignature_ChangesWithConfig(t *testing.T) {
	ctx := context.Background()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data:       map[string]string{config.KeyNodeAttachments: "one"},
	}
	clientset := fake.NewSimpleClientset(cm)
	opts := Options{Clientset: clientset, ManagerNamespace: "kubevpn"}
	one, err := attachmentSignature(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := attachmentSignature(ctx, opts)
	if one != again {
		t.Fatal("same config should have same signature")
	}
	cm.Data[config.KeyNodeAttachments] = "two"
	_, _ = clientset.CoreV1().ConfigMaps("kubevpn").Update(ctx, cm, metav1.UpdateOptions{})
	two, _ := attachmentSignature(ctx, opts)
	if one == two {
		t.Fatal("changed config should have different signature")
	}
}
