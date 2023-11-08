package util

import (
	"context"
	"encoding/json"
	"os"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

func GetClusterId(client v12.ConfigMapInterface) (types.UID, error) {
	a, err := client.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		err = errors.Wrap(err, "client.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{}): ")
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
		err = errors.Wrap(err, "client.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{}): ")
		return false, err
	}
	var b *corev1.ConfigMap
	b, err = clientB.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		err = errors.Wrap(err, "clientB.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{}): ")
		return false, err
	}
	return a.UID == b.UID, nil
}

func ConvertToKubeconfigBytes(factory cmdutil.Factory) ([]byte, string, error) {
	loader := factory.ToRawKubeConfigLoader()
	namespace, _, err := loader.Namespace()
	if err != nil {
		err = errors.Wrap(err, "loader.Namespace(): ")
		return nil, "", err
	}
	rawConfig, err := loader.RawConfig()
	err = api.FlattenConfig(&rawConfig)
	if err != nil {
		err = errors.Wrap(err, "api.FlattenConfig(&rawConfig): ")
		return nil, "", err
	}
	convertedObj, err := latest.Scheme.ConvertToVersion(&rawConfig, latest.ExternalVersion)
	if err != nil {
		err = errors.Wrap(err, "latest.Scheme.ConvertToVersion(&rawConfig, latest.ExternalVersion): ")
		return nil, "", err
	}
	marshal, err := json.Marshal(convertedObj)
	if err != nil {
		err = errors.Wrap(err, "json.Marshal(convertedObj): ")
		return nil, "", err
	}
	return marshal, namespace, nil
}

func ConvertToTempKubeconfigFile(kubeconfigBytes []byte) (string, error) {
	temp, err := os.CreateTemp("", "*.tmp.kubeconfig")
	if err != nil {
		err = errors.Wrap(err, "os.CreateTemp(\"\", \"*.tmp.kubeconfig\"): ")
		return "", err
	}
	err = temp.Close()
	if err != nil {
		err = errors.Wrap(err, "temp.Close(): ")
		return "", err
	}
	err = os.WriteFile(temp.Name(), kubeconfigBytes, os.ModePerm)
	if err != nil {
		err = errors.Wrap(err, "os.WriteFile(temp.Name(), kubeconfigBytes, os.ModePerm): ")
		return "", err
	}
	return temp.Name(), nil
}
