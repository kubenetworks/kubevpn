package handler

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes/fake"
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
