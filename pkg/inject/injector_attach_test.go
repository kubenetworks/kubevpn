package inject

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestNewInjectorSelectsNodeAgentForNonService(t *testing.T) {
	opts := minimalInjectOptionsForTest()
	opts.AttachMode = config.AttachModeNodeAgent
	inj := NewInjector(opts)
	if _, ok := inj.(*nodeAgentInjector); !ok {
		t.Fatalf("want nodeAgentInjector for non-Service + node-agent mode, got %T", inj)
	}
}

func TestNewInjectorSelectsMeshByDefault(t *testing.T) {
	opts := minimalInjectOptionsForTest()
	inj := NewInjector(opts)
	if _, ok := inj.(*meshInjector); !ok {
		t.Fatalf("want meshInjector by default, got %T", inj)
	}
}

func TestNewInjectorSelectsFargateForServiceEvenInNodeAgentMode(t *testing.T) {
	opts := minimalInjectOptionsForTest()
	opts.AttachMode = config.AttachModeNodeAgent
	opts.Object = serviceObjectForTest()
	inj := NewInjector(opts)
	if _, ok := inj.(*fargateInjector); !ok {
		t.Fatalf("Service target must keep fargateInjector regardless of mode, got %T", inj)
	}
}

// minimalInjectOptionsForTest builds the minimal non-Service InjectOptions the
// factory needs.
func minimalInjectOptionsForTest() InjectOptions {
	return InjectOptions{
		Object: &resource.Info{
			Mapping: &meta.RESTMapping{
				Resource: schema.GroupVersionResource{Resource: "deployments"},
			},
		},
	}
}

func serviceObjectForTest() *resource.Info {
	svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"}}
	return &resource.Info{Object: svc, Mapping: &meta.RESTMapping{Resource: schema.GroupVersionResource{Resource: "services"}}, Namespace: "ns", Name: "svc"}
}
