package nodeagent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

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
