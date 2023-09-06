package util

import (
	"context"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

func GetClusterId(client v12.ConfigMapInterface) (types.UID, error) {
	a, err := client.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return a.UID, nil
}

func IsSameCluster(client v12.ConfigMapInterface, namespace string, clientB v12.ConfigMapInterface, namespaceB string) (bool, error) {
	if namespace != namespaceB {
		return false, nil
	}
	ctx := context.Background()
	a, err := client.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	var b *corev1.ConfigMap
	b, err = clientB.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return a.UID == b.UID, nil
}

func ConvertToKubeconfigBytes(factory cmdutil.Factory) ([]byte, error) {
	rawConfig, err := factory.ToRawKubeConfigLoader().RawConfig()
	convertedObj, err := latest.Scheme.ConvertToVersion(&rawConfig, latest.ExternalVersion)
	if err != nil {
		return nil, err
	}
	return json.Marshal(convertedObj)
}
