package inject

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
)

func TestNewInjectorSelectsNodeAgentForWorkloadAndService(t *testing.T) {
	for _, object := range []*resource.Info{minimalInjectOptionsForTest().Object, serviceObjectForTest()} {
		inj := NewInjector(InjectOptions{Object: object})
		if _, ok := inj.(*nodeAgentInjector); !ok {
			t.Fatalf("want nodeAgentInjector, got %T", inj)
		}
	}
}

func minimalInjectOptionsForTest() InjectOptions {
	return InjectOptions{Object: &resource.Info{Mapping: &meta.RESTMapping{Resource: schema.GroupVersionResource{Resource: "deployments"}}}}
}

func serviceObjectForTest() *resource.Info {
	svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"}}
	return &resource.Info{Object: svc, Mapping: &meta.RESTMapping{Resource: schema.GroupVersionResource{Resource: "services"}}, Namespace: "ns", Name: "svc"}
}
