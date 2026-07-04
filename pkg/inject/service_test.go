package inject

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

func TestModifyServiceTargetPort(t *testing.T) {
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "reviews", Namespace: "default"},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{Port: 9080, TargetPort: intstr.FromInt32(9080)},
				{Port: 8080, TargetPort: intstr.FromInt32(8080)},
			},
		},
	}
	clientset := fake.NewSimpleClientset(svc)

	m := map[int32]int32{9080: 38721, 8080: 41234}
	err := ModifyServiceTargetPort(context.Background(), clientset, "default", "reviews", m)
	if err != nil {
		t.Fatalf("ModifyServiceTargetPort: %v", err)
	}

	updated, err := clientset.CoreV1().Services("default").Get(context.Background(), "reviews", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Spec.Ports[0].TargetPort.IntVal != 38721 {
		t.Fatalf("port 9080: want targetPort=38721, got %d", updated.Spec.Ports[0].TargetPort.IntVal)
	}
	if updated.Spec.Ports[1].TargetPort.IntVal != 41234 {
		t.Fatalf("port 8080: want targetPort=41234, got %d", updated.Spec.Ports[1].TargetPort.IntVal)
	}
}

func TestModifyServiceTargetPort_Restore(t *testing.T) {
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "reviews", Namespace: "default"},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{Port: 9080, TargetPort: intstr.FromInt32(38721)},
			},
		},
	}
	clientset := fake.NewSimpleClientset(svc)

	// Empty map restores targetPort to equal port
	err := ModifyServiceTargetPort(context.Background(), clientset, "default", "reviews", map[int32]int32{})
	if err != nil {
		t.Fatalf("ModifyServiceTargetPort restore: %v", err)
	}

	updated, _ := clientset.CoreV1().Services("default").Get(context.Background(), "reviews", metav1.GetOptions{})
	if updated.Spec.Ports[0].TargetPort.IntVal != 9080 {
		t.Fatalf("restore: want targetPort=9080, got %d", updated.Spec.Ports[0].TargetPort.IntVal)
	}
}

func TestModifyServiceTargetPort_NotFound(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	err := ModifyServiceTargetPort(context.Background(), clientset, "default", "nonexistent", map[int32]int32{80: 12345})
	if err == nil {
		t.Fatal("expected error for nonexistent service")
	}
}
