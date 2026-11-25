package xds

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

const publisherVirtualsYAML = `- schemaVersion: 2
  Namespace: default
  Uid: deployments.apps.reviews
  selector: app=reviews
  Rules:
  - ownerID: owner-1
    LocalTunIPv4: 198.18.0.1
    isAgent: true
`

func newPublisherTestPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "reviews-x", Namespace: "default", UID: "uid-1", Labels: map[string]string{"app": "reviews"}},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.0.5"},
	}
}

func TestAttachmentPublisher_PublishesAndRemoves(t *testing.T) {
	ctx := context.Background()
	pod := newPublisherTestPod()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data:       map[string]string{config.KeyEnvoy: publisherVirtualsYAML},
	}
	clientset := fake.NewSimpleClientset(cm, pod)
	p := NewAttachmentPublisher(clientset, "kubevpn")
	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var attachments []controlplane.NodeAttachment
	if err := yaml.Unmarshal([]byte(got.Data[config.KeyNodeAttachments]), &attachments); err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 1 || attachments[0].PodUID != "uid-1" || attachments[0].NodeName != "node-a" || attachments[0].PodIP != "10.244.0.5" {
		t.Fatalf("unexpected attachments: %+v", attachments)
	}
	if attachments[0].AgentListenerPort == 0 {
		t.Fatal("listener port not allocated")
	}
	var virtuals []*controlplane.Virtual
	if err := yaml.Unmarshal([]byte(got.Data[config.KeyEnvoy]), &virtuals); err != nil {
		t.Fatal(err)
	}
	if virtuals[0].AgentListenerPort != attachments[0].AgentListenerPort {
		t.Fatal("attachment port differs from Virtual port")
	}

	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	gotAgain, _ := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	var virtualsAgain []*controlplane.Virtual
	_ = yaml.Unmarshal([]byte(gotAgain.Data[config.KeyEnvoy]), &virtualsAgain)
	if virtualsAgain[0].AgentListenerPort != virtuals[0].AgentListenerPort {
		t.Fatal("port changed on second reconcile")
	}

	_ = clientset.CoreV1().Pods("default").Delete(ctx, pod.Name, metav1.DeleteOptions{})
	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	attachments = nil
	_ = yaml.Unmarshal([]byte(got.Data[config.KeyNodeAttachments]), &attachments)
	if len(attachments) != 0 {
		t.Fatalf("stale attachments: %+v", attachments)
	}
}

func TestAttachmentPublisher_SkipsSidecarPod(t *testing.T) {
	ctx := context.Background()
	pod := newPublisherTestPod()
	pod.Spec.Containers = []corev1.Container{{Name: config.ContainerSidecarVPN}, {Name: config.ContainerSidecarEnvoy}}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data:       map[string]string{config.KeyEnvoy: publisherVirtualsYAML},
	}
	clientset := fake.NewSimpleClientset(cm, pod)
	p := NewAttachmentPublisher(clientset, "kubevpn")
	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	var attachments []controlplane.NodeAttachment
	_ = yaml.Unmarshal([]byte(got.Data[config.KeyNodeAttachments]), &attachments)
	if len(attachments) != 0 {
		t.Fatalf("sidecar pod must not get attachment: %+v", attachments)
	}
}
