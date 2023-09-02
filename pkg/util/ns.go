package util

import (
	"context"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
)

func GetNamespaceId(namespaceInterface v12.NamespaceInterface, ns string) (types.UID, error) {
	namespace, err := namespaceInterface.Get(context.Background(), ns, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return namespace.UID, nil
}
