package kubevpn

import (
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestName(t *testing.T) {
	clientset := fake.NewClientset()
	if clientset == nil {
		t.Fatalf("Failed to create clientset")
	}
}
