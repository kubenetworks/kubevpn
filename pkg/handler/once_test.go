package handler

import (
	"context"
	"sync/atomic"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestLabelNs(t *testing.T) {
	ctx := context.Background()
	namespace := "test-ns"
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		},
	)

	err := labelNs(ctx, namespace, clientset)
	if err != nil {
		t.Fatalf("labelNs returned unexpected error: %v", err)
	}

	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get namespace after labelNs: %v", err)
	}
	if ns.Labels == nil {
		t.Fatal("expected labels to be set, got nil")
	}
	if ns.Labels["ns"] != namespace {
		t.Fatalf("expected label ns=%q, got %q", namespace, ns.Labels["ns"])
	}
}

func TestLabelNs_AlreadyLabeled(t *testing.T) {
	ctx := context.Background()
	namespace := "test-ns"
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   namespace,
				Labels: map[string]string{"ns": namespace},
			},
		},
	)

	err := labelNs(ctx, namespace, clientset)
	if err != nil {
		t.Fatalf("labelNs returned unexpected error for already-labeled namespace: %v", err)
	}

	// Verify the label is still correct
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get namespace: %v", err)
	}
	if ns.Labels["ns"] != namespace {
		t.Fatalf("expected label ns=%q, got %q", namespace, ns.Labels["ns"])
	}
}

func TestCreateOutboundPod(t *testing.T) {
	ctx := context.Background()
	namespace := "test-ns"
	image := "ghcr.io/kubenetworks/kubevpn:test"
	imagePullSecretName := ""

	// Pre-create the namespace so createOutboundPod can label it.
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		},
	)

	// Use a reactor to control pod list behavior:
	// - First call (from DetectPodExists): return empty list so creation proceeds
	// - Subsequent calls (from WaitPodReady): return a ready pod so wait completes
	var podListCallCount int32
	clientset.PrependReactor("list", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		count := atomic.AddInt32(&podListCallCount, 1)
		if count == 1 {
			// DetectPodExists: no pods exist yet
			return true, &v1.PodList{Items: []v1.Pod{}}, nil
		}
		// WaitPodReady: return a ready pod
		return true, &v1.PodList{
			Items: []v1.Pod{{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.ConfigMapPodTrafficManager + "-abc123",
					Namespace: namespace,
					Labels:    map[string]string{"app": config.ConfigMapPodTrafficManager},
				},
				Status: v1.PodStatus{
					Phase: v1.PodRunning,
					Conditions: []v1.PodCondition{{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					}},
					ContainerStatuses: []v1.ContainerStatus{
						{Name: config.ContainerSidecarVPN, Ready: true},
						{Name: config.ContainerSidecarXDS, Ready: true},
						{Name: config.ContainerSidecarDNS, Ready: true},
					},
				},
			}},
		}, nil
	})

	err := createOutboundPod(ctx, clientset, namespace, image, imagePullSecretName)
	if err != nil {
		t.Fatalf("createOutboundPod returned unexpected error: %v", err)
	}

	// Verify Deployment was created
	deploy, err := clientset.AppsV1().Deployments(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected deployment to be created, got error: %v", err)
	}
	if deploy.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected deployment name %q, got %q", config.ConfigMapPodTrafficManager, deploy.Name)
	}
	if deploy.Spec.Template.Spec.Containers[0].Image != image {
		t.Fatalf("expected image %q, got %q", image, deploy.Spec.Template.Spec.Containers[0].Image)
	}

	// Verify Service was created
	svc, err := clientset.CoreV1().Services(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected service to be created, got error: %v", err)
	}
	if svc.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected service name %q, got %q", config.ConfigMapPodTrafficManager, svc.Name)
	}
	if len(svc.Spec.Ports) != 3 {
		t.Fatalf("expected 3 service ports, got %d", len(svc.Spec.Ports))
	}

	// Verify Secret was created
	secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected secret to be created, got error: %v", err)
	}
	if secret.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected secret name %q, got %q", config.ConfigMapPodTrafficManager, secret.Name)
	}
	if len(secret.Data[config.TLSCertKey]) == 0 {
		t.Fatal("expected TLS cert data in secret, got empty")
	}
	if len(secret.Data[config.TLSPrivateKeyKey]) == 0 {
		t.Fatal("expected TLS key data in secret, got empty")
	}

	// Verify ServiceAccount was created
	sa, err := clientset.CoreV1().ServiceAccounts(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected service account to be created, got error: %v", err)
	}
	if sa.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected service account name %q, got %q", config.ConfigMapPodTrafficManager, sa.Name)
	}

	// Verify Role was created
	role, err := clientset.RbacV1().Roles(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected role to be created, got error: %v", err)
	}
	if role.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected role name %q, got %q", config.ConfigMapPodTrafficManager, role.Name)
	}

	// Verify RoleBinding was created
	rb, err := clientset.RbacV1().RoleBindings(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected role binding to be created, got error: %v", err)
	}
	if rb.Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("expected role binding name %q, got %q", config.ConfigMapPodTrafficManager, rb.Name)
	}

	// Verify namespace was labeled
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get namespace: %v", err)
	}
	if ns.Labels["ns"] != namespace {
		t.Fatalf("expected namespace label ns=%q, got %q", namespace, ns.Labels["ns"])
	}
}
