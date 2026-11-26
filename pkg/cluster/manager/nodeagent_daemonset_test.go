package manager

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestGenNodeAgentDaemonSet_SecurityContract(t *testing.T) {
	ds := genNodeAgentDaemonSet("kubevpn", "test-image")
	podSpec := ds.Spec.Template.Spec
	if podSpec.HostNetwork {
		t.Fatal("hostNetwork must be disabled")
	}
	if !podSpec.HostPID {
		t.Fatal("hostPID is required to reach host netns")
	}
	if podSpec.ServiceAccountName != config.ConfigMapPodTrafficManager {
		t.Fatalf("serviceAccountName = %q, want %q", podSpec.ServiceAccountName, config.ConfigMapPodTrafficManager)
	}
	if len(podSpec.Containers) != 1 {
		t.Fatal("expected one node-agent container")
	}
	container := podSpec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.Privileged == nil || !*container.SecurityContext.Privileged {
		t.Fatal("existing node-agent privilege level is required for host netns access")
	}
	if container.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Fatal("CI-loaded images must not force a registry pull")
	}
	if len(podSpec.Volumes) != 1 || podSpec.Volumes[0].HostPath == nil || podSpec.Volumes[0].HostPath.Path != "/proc" {
		t.Fatal("host /proc mount missing")
	}
	if ds.Name != nodeAgentDaemonSetName {
		t.Fatalf("unexpected daemonset name: %s", ds.Name)
	}
}

func TestEnsureNodeAgentDaemonSet_CreatesWhenMissing(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn", UID: "sa-1"}},
	)
	if err := ensureNodeAgentDaemonSet(ctx, clientset, "kubevpn", "test-image"); err != nil {
		t.Fatal(err)
	}
	if _, err := clientset.AppsV1().DaemonSets("kubevpn").Get(ctx, nodeAgentDaemonSetName, metav1.GetOptions{}); err != nil {
		t.Fatalf("daemonset was not created: %v", err)
	}
}

func TestEnsureNodeAgentDaemonSet_UpdatesLegacyServiceAccount(t *testing.T) {
	ctx := context.Background()
	existing := genNodeAgentDaemonSet("kubevpn", "test-image")
	existing.Spec.Template.Spec.ServiceAccountName = "default"
	clientset := fake.NewSimpleClientset(
		existing,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn", UID: "sa-1"}},
	)
	if err := ensureNodeAgentDaemonSet(ctx, clientset, "kubevpn", "test-image"); err != nil {
		t.Fatal(err)
	}
	updated, err := clientset.AppsV1().DaemonSets("kubevpn").Get(ctx, nodeAgentDaemonSetName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Spec.Template.Spec.ServiceAccountName; got != config.ConfigMapPodTrafficManager {
		t.Fatalf("serviceAccountName = %q, want %q", got, config.ConfigMapPodTrafficManager)
	}
}

func TestEnsureNodeAgentDaemonSet_RestartsWhenServiceAccountIsRecreated(t *testing.T) {
	ctx := context.Background()
	existing := genNodeAgentDaemonSet("kubevpn", "test-image")
	existing.Spec.Template.Annotations = map[string]string{nodeAgentServiceAccountUIDAnnotation: "old-sa-uid"}
	clientset := fake.NewSimpleClientset(
		existing,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn", UID: "new-sa-uid"}},
	)
	if err := ensureNodeAgentDaemonSet(ctx, clientset, "kubevpn", "test-image"); err != nil {
		t.Fatal(err)
	}
	updated, err := clientset.AppsV1().DaemonSets("kubevpn").Get(ctx, nodeAgentDaemonSetName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Spec.Template.Annotations[nodeAgentServiceAccountUIDAnnotation]; got != "new-sa-uid" {
		t.Fatalf("service account UID annotation = %q, want new-sa-uid", got)
	}
}
