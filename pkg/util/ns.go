package util

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"reflect"
	"unsafe"

	errors2 "github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func GetConnectionID(ctx context.Context, client v12.NamespaceInterface, ns string) (string, error) {
	namespace, err := client.Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return string(namespace.UID[len(namespace.UID)-12:]), nil
}

func IsSameConnection(ctx context.Context, clientA v12.CoreV1Interface, namespaceA string, clientB v12.CoreV1Interface, namespaceB string) (bool, error) {
	if namespaceA != namespaceB {
		return false, nil
	}
	connectionA, err := GetConnectionID(ctx, clientA.Namespaces(), namespaceA)
	if err != nil {
		return false, err
	}
	connectionB, err := GetConnectionID(ctx, clientB.Namespaces(), namespaceB)
	if err != nil {
		return false, err
	}
	return connectionA == connectionB, nil
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

func GetAPIServerFromKubeConfigBytes(kubeconfigBytes []byte) *net.IPNet {
	kubeConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return nil
	}
	var host string
	host, _, err = net.SplitHostPort(kubeConfig.Host)
	if err != nil {
		u, err2 := url.Parse(kubeConfig.Host)
		if err2 != nil {
			return nil
		}
		host, _, err = net.SplitHostPort(u.Host)
		if err != nil {
			return nil
		}
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	var mask net.IPMask
	if ip.To4() != nil {
		mask = net.CIDRMask(32, 32)
	} else {
		mask = net.CIDRMask(128, 128)
	}
	return &net.IPNet{IP: ip, Mask: mask}
}

func ConvertToTempKubeconfigFile(kubeconfigBytes []byte, path string) (string, error) {
	var f *os.File
	var err error

	if path == "" {
		path = GenKubeconfigTempPath(kubeconfigBytes)
	}
	f, err = os.Create(path)
	if err != nil {
		return "", err
	}
	_, err = f.Write(kubeconfigBytes)
	if err != nil {
		return "", err
	}
	err = f.Chmod(0644)
	if err != nil {
		return "", err
	}
	err = f.Close()
	if err != nil {
		return "", err
	}
	return f.Name(), nil
}

func InitFactory(kubeconfigBytes string, ns string) cmdutil.Factory {
	configFlags := genericclioptions.NewConfigFlags(true)
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
	file, err := ConvertToTempKubeconfigFile([]byte(kubeconfigBytes), "")
	if err != nil {
		return nil
	}
	configFlags.KubeConfig = pointer.String(file)
	configFlags.Namespace = pointer.String(ns)
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	return cmdutil.NewFactory(matchVersionFlags)
}

func InitFactoryByPath(kubeconfig string, ns string) cmdutil.Factory {
	configFlags := genericclioptions.NewConfigFlags(true)
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

	file, err := ConvertToTempKubeconfigFile(kubeconfigJsonBytes, "")
	if err != nil {
		return "", err
	}
	return file, nil
}

func GetNsForListPodAndSvc(ctx context.Context, clientset *kubernetes.Clientset, nsList []string) (podNs string, svcNs string, err error) {
	for _, ns := range nsList {
		_, err = clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{Limit: 1})
		if errors.IsForbidden(err) {
			continue
		}
		if err != nil {
			return
		}
		podNs = ns
		break
	}
	if err != nil {
		err = errors2.Wrap(err, "can not list pod to add it to route table")
		return
	}
	if podNs == "" {
		log.G(ctx).Debugf("List all namepsace pods")
	} else {
		log.G(ctx).Debugf("List namepsace %s pods", podNs)
	}

	for _, ns := range nsList {
		_, err = clientset.CoreV1().Services(ns).List(ctx, metav1.ListOptions{Limit: 1})
		if errors.IsForbidden(err) {
			continue
		}
		if err != nil {
			return
		}
		svcNs = ns
		break
	}
	if err != nil {
		err = errors2.Wrap(err, "can not list service to add it to route table")
		return
	}
	if svcNs == "" {
		log.G(ctx).Debugf("List all namepsace services")
	} else {
		log.G(ctx).Debugf("List namepsace %s services", svcNs)
	}
	return
}
