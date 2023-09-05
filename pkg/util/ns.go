package util

import (
	"context"
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

func GetNamespaceId(namespaceInterface v12.NamespaceInterface, ns string) (types.UID, error) {
	namespace, err := namespaceInterface.Get(context.Background(), ns, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return namespace.UID, nil
}

func ConvertToKubeconfigBytes(factory cmdutil.Factory) ([]byte, error) {
	rawConfig, err := factory.ToRawKubeConfigLoader().RawConfig()
	convertedObj, err := latest.Scheme.ConvertToVersion(&rawConfig, latest.ExternalVersion)
	if err != nil {
		return nil, err
	}
	return json.Marshal(convertedObj)
}
