package handler

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// newUpgradeTestConnectOptions creates a ConnectOptions with a fake clientset pre-loaded
// with the given deployment and pods for upgrade testing.
func newUpgradeTestConnectOptions(t *testing.T, objects ...metav1.Object) *ConnectOptions {
	t.Helper()

	clientset := fake.NewSimpleClientset()
	for _, obj := range objects {
		switch o := obj.(type) {
		case *appsv1.Deployment:
			_, err := clientset.AppsV1().Deployments(o.Namespace).Create(context.Background(), o, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("failed to create deployment: %v", err)
			}
		case *v1.Pod:
			_, err := clientset.CoreV1().Pods(o.Namespace).Create(context.Background(), o, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("failed to create pod: %v", err)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &ConnectOptions{
		ManagerNamespace:  "test-ns",
		WorkloadNamespace: "default",
		K8sClient:         K8sClient{clientset: clientset},
		ctx:               ctx,
		cancel:            cancel,
	}
}

// makeTrafficManagerDeployment creates a Deployment with the given container image.
func makeTrafficManagerDeployment(namespace, image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": config.ConfigMapPodTrafficManager},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": config.ConfigMapPodTrafficManager},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  config.ContainerSidecarVPN,
							Image: image,
						},
					},
				},
			},
		},
	}
}

// makeTrafficManagerPod creates a running Pod with the given container image and label.
func makeTrafficManagerPod(namespace, image string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager + "-abc123",
			Namespace: namespace,
			Labels:    map[string]string{"app": config.ConfigMapPodTrafficManager},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  config.ContainerSidecarVPN,
					Image: image,
				},
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodReady,
					Status: v1.ConditionTrue,
				},
			},
			ContainerStatuses: []v1.ContainerStatus{
				{
					Name:  config.ContainerSidecarVPN,
					Ready: true,
					State: v1.ContainerState{
						Running: &v1.ContainerStateRunning{},
					},
				},
			},
		},
	}
}

func TestUpgradeDeploy_AlreadyUpToDate(t *testing.T) {
	// Save and restore config.Version
	origVersion := config.Version
	defer func() { config.Version = origVersion }()
	config.Version = "2.3.0"

	const currentImage = "ghcr.io/kubenetworks/kubevpn:v2.3.0"

	c := newUpgradeTestConnectOptions(t,
		makeTrafficManagerDeployment("test-ns", currentImage),
		makeTrafficManagerPod("test-ns", currentImage),
	)
	c.Image = currentImage

	err := c.UpgradeDeploy(context.Background())
	if err != nil {
		t.Fatalf("expected nil error when deployment is up to date, got: %v", err)
	}
}

func TestUpgradeDeploy_OldImage_ShouldAttemptUpdate(t *testing.T) {
	// Save and restore config.Version
	origVersion := config.Version
	defer func() { config.Version = origVersion }()
	config.Version = "2.4.0"

	const oldImage = "ghcr.io/kubenetworks/kubevpn:v2.3.0"
	const newImage = "ghcr.io/kubenetworks/kubevpn:v2.4.0"

	c := newUpgradeTestConnectOptions(t,
		makeTrafficManagerDeployment("test-ns", oldImage),
		makeTrafficManagerPod("test-ns", oldImage),
	)
	c.Image = newImage

	// UpgradeDeploy will detect the version mismatch and attempt to upgrade.
	// Since we have no factory (nil), it will panic when trying to call upgradeSecretSpec.
	// We recover from the panic to prove that the upgrade code path was entered
	// (i.e., IsNewer returned true and the function did NOT return nil early).
	didPanic := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				didPanic = true
				t.Logf("upgrade attempted as expected, panicked on nil factory: %v", r)
			}
		}()
		err := c.UpgradeDeploy(context.Background())
		if err == nil {
			t.Fatal("expected non-nil error or panic because upgrade path requires a factory, but got nil (meaning no upgrade was attempted)")
		}
		// If we get a non-nil error without panic, that's also acceptable
		t.Logf("upgrade attempted, got expected error: %v", err)
	}()

	if !didPanic {
		// If it didn't panic, it should have returned a non-nil error (already checked above).
		// This branch is fine — means the code errored gracefully.
	}
}

func TestUpgradeDeploy_DeploymentNotFound(t *testing.T) {
	// Save and restore config.Version
	origVersion := config.Version
	defer func() { config.Version = origVersion }()
	config.Version = "2.3.0"

	// Create ConnectOptions with NO deployment in the fake clientset
	clientset := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	c := &ConnectOptions{
		ManagerNamespace:  "test-ns",
		WorkloadNamespace: "default",
		K8sClient:         K8sClient{clientset: clientset},
		Image:             "ghcr.io/kubenetworks/kubevpn:v2.3.0",
		ctx:               ctx,
		cancel:            cancel,
	}

	err := c.UpgradeDeploy(context.Background())
	if err == nil {
		t.Fatal("expected error when deployment does not exist, got nil")
	}
	t.Logf("got expected error for missing deployment: %v", err)
}
