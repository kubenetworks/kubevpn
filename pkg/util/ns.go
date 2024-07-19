package util

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"unsafe"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func GetClusterID(ctx context.Context, client v12.ConfigMapInterface) (types.UID, error) {
	configMap, err := client.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return configMap.UID, nil
}

func GetClusterIDByCM(cm *v1.ConfigMap) types.UID {
	return cm.UID
}

func IsSameCluster(client v12.ConfigMapInterface, namespace string, clientB v12.ConfigMapInterface, namespaceB string) (bool, error) {
	if namespace != namespaceB {
		return false, nil
	}
	ctx := context.Background()
	clusterIDA, err := GetClusterID(ctx, client)
	if err != nil {
		return false, err
	}
	var clusterIDB types.UID
	clusterIDB, err = GetClusterID(ctx, clientB)
	if err != nil {
		return false, err
	}
	return clusterIDA == clusterIDB, nil
}

func ConvertToKubeConfigBytes(factory cmdutil.Factory) ([]byte, string, error) {
	loader := factory.ToRawKubeConfigLoader()
	namespace, _, err := loader.Namespace()
	if err != nil {
		return nil, "", err
	}
	// todo: use more elegant way to get MergedRawConfig
	var useReflectToGetRawConfigFunc = func() (c api.Config, err error) {
		defer func() {
			if er := recover(); er != nil {
				err = er.(error)
			}
		}()
		value := reflect.ValueOf(loader).Elem().Field(0)
		value = reflect.NewAt(value.Type(), unsafe.Pointer(value.UnsafeAddr())).Elem()
		loadingClientConfig := value.Interface().(*clientcmd.DeferredLoadingClientConfig)
		value = reflect.ValueOf(loadingClientConfig).Elem().Field(3)
		value = reflect.NewAt(value.Type(), unsafe.Pointer(value.UnsafeAddr())).Elem()
		clientConfig := value.Interface().(*clientcmd.DirectClientConfig)
		return clientConfig.MergedRawConfig()
	}
	rawConfig, err := useReflectToGetRawConfigFunc()
	if err != nil {
		rawConfig, err = loader.RawConfig()
	}
	if err != nil {
		return nil, "", err
	}
	err = api.FlattenConfig(&rawConfig)
	if err != nil {
		return nil, "", err
	}
	convertedObj, err := latest.Scheme.ConvertToVersion(&rawConfig, latest.ExternalVersion)
	if err != nil {
		return nil, "", err
	}
	marshal, err := json.Marshal(convertedObj)
	if err != nil {
		return nil, "", err
	}
	return marshal, namespace, nil
}

func ConvertToTempKubeconfigFile(kubeconfigBytes []byte) (string, error) {
	temp, err := os.CreateTemp("", "*.kubeconfig")
	if err != nil {
		return "", err
	}
	err = temp.Close()
	if err != nil {
		return "", err
	}
	err = os.WriteFile(temp.Name(), kubeconfigBytes, os.ModePerm)
	if err != nil {
		return "", err
	}
	return temp.Name(), nil
}

func InitFactory(kubeconfigBytes string, ns string) cmdutil.Factory {
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.WrapConfigFn = func(c *rest.Config) *rest.Config {
		if path, ok := os.LookupEnv(config.EnvSSHJump); ok {
			bytes, err := os.ReadFile(path)
			cmdutil.CheckErr(err)
			var conf *rest.Config
			conf, err = clientcmd.RESTConfigFromKubeConfig(bytes)
			cmdutil.CheckErr(err)
			return conf
		}
		return c
	}
	temp, err := os.CreateTemp("", "*.kubeconfig")
	if err != nil {
		return nil
	}
	err = temp.Close()
	if err != nil {
		return nil
	}
	err = os.WriteFile(temp.Name(), []byte(kubeconfigBytes), os.ModePerm)
	if err != nil {
		return nil
	}
	configFlags.KubeConfig = pointer.String(temp.Name())
	configFlags.Namespace = pointer.String(ns)
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	return cmdutil.NewFactory(matchVersionFlags)
}

func InitFactoryByPath(kubeconfig string, ns string) cmdutil.Factory {
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.KubeConfig = pointer.String(kubeconfig)
	configFlags.Namespace = pointer.String(ns)
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	return cmdutil.NewFactory(matchVersionFlags)
}

func GetKubeconfigCluster(f cmdutil.Factory) string {
	rawConfig, err := f.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return ""
	}
	if rawConfig.Contexts != nil && rawConfig.Contexts[rawConfig.CurrentContext] != nil {
		return rawConfig.Contexts[rawConfig.CurrentContext].Cluster
	}
	return ""
}

func GetKubeconfigPath(factory cmdutil.Factory) (string, error) {
	rawConfig, err := factory.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return "", err
	}
	err = api.FlattenConfig(&rawConfig)
	if err != nil {
		return "", err
	}
	rawConfig.SetGroupVersionKind(schema.GroupVersionKind{Version: latest.Version, Kind: "Config"})
	var convertedObj runtime.Object
	convertedObj, err = latest.Scheme.ConvertToVersion(&rawConfig, latest.ExternalVersion)
	if err != nil {
		return "", err
	}
	var kubeconfigJsonBytes []byte
	kubeconfigJsonBytes, err = json.Marshal(convertedObj)
	if err != nil {
		return "", err
	}

	temp, err := os.CreateTemp("", "*.kubeconfig")
	if err != nil {
		return "", err
	}
	temp.Close()
	err = os.WriteFile(temp.Name(), kubeconfigJsonBytes, 0644)
	if err != nil {
		return "", err
	}
	err = os.Chmod(temp.Name(), 0644)
	if err != nil {
		return "", err
	}

	return temp.Name(), nil
}
