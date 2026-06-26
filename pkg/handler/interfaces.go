package handler

import (
	"context"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ConfigMapInterface abstracts ConfigMap operations for testability.
type ConfigMapInterface interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1.ConfigMap, error)
	Update(ctx context.Context, configMap *v1.ConfigMap, opts metav1.UpdateOptions) (*v1.ConfigMap, error)
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*v1.ConfigMap, error)
}

// SecretInterface abstracts Secret operations for testability.
type SecretInterface interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1.Secret, error)
}

// PodInterface abstracts Pod operations for testability.
type PodInterface interface {
	List(ctx context.Context, opts metav1.ListOptions) (*v1.PodList, error)
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1.Pod, error)
}
