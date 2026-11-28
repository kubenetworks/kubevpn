package inject

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestNodeAgentInjector_RejectsUnsupportedService(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*corev1.Service)
		wantMessage string
	}{
		{name: "headless", mutate: func(service *corev1.Service) { service.Spec.ClusterIP = corev1.ClusterIPNone }, wantMessage: "headless"},
		{name: "selector-less", mutate: func(service *corev1.Service) { service.Spec.Selector = nil }, wantMessage: "without a selector"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := &corev1.Service{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
				ObjectMeta: metav1.ObjectMeta{Name: "reviews", Namespace: "default"},
				Spec:       corev1.ServiceSpec{ClusterIP: "10.96.0.10", Selector: map[string]string{"app": "foo"}},
			}
			tt.mutate(service)
			serviceObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(service)
			if err != nil {
				t.Fatal(err)
			}
			deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "default"}}
			controllerObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(deployment)
			if err != nil {
				t.Fatal(err)
			}
			opts := InjectOptions{
				Clientset:        fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kubevpn"}}),
				ManagerNamespace: "kubevpn",
				NodeID:           "services.reviews",
				LocalTunIPv4:     "198.18.0.1",
				OwnerID:          "owner-a",
				Object: &resource.Info{
					Object: &unstructured.Unstructured{Object: serviceObject}, Namespace: "default", Name: "reviews",
					Mapping: &meta.RESTMapping{Resource: schema.GroupVersionResource{Resource: "services"}},
				},
				Controller: &resource.Info{
					Object: &unstructured.Unstructured{Object: controllerObject}, Namespace: "default", Name: "foo",
					Mapping: &meta.RESTMapping{Resource: schema.GroupVersionResource{Resource: "deployments"}},
				},
			}
			err = NewInjector(opts).Inject(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("expected %q rejection, got %v", tt.wantMessage, err)
			}
			if _, err := opts.Clientset.CoreV1().ConfigMaps("kubevpn").Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{}); err == nil {
				t.Fatal("rejected Service unexpectedly wrote manager state")
			}
		})
	}
}
