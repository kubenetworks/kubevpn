package util

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"reflect"
	"unsafe"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// GetConnectionID returns the last 12 characters of the namespace UID as a connection identifier.
func GetConnectionID(ctx context.Context, client v12.NamespaceInterface, ns string) (string, error) {
	namespace, err := client.Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return string(namespace.UID[len(namespace.UID)-12:]), nil
}

// ConvertToKubeConfigBytes extracts the flattened kubeconfig as JSON bytes and the namespace from the factory.
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

// GetAPIServerFromKubeConfigBytes parses kubeconfig bytes and returns the API server IP as a /32 or /128 IPNet.
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

// ConvertToTempKubeconfigFile writes kubeconfig bytes to a temp file and returns the path.
func ConvertToTempKubeconfigFile(kubeconfigBytes []byte, path string) (string, error) {
	var f *os.File
	var err error
	if path == "" {
		// os.CreateTemp guarantees an atomically-unique name so concurrent writers
		// (root daemon Connect + user daemon Sync) for the same cluster never derive
		// the same path and race on truncating a file owned by another user.
		f, err = os.CreateTemp(config.GetTempPath(), GenKubeconfigTempPattern(kubeconfigBytes))
	} else {
		f, err = os.Create(path)
	}
	if err != nil {
		return "", err
	}
	// Safety net: close on every error path; the explicit Close below surfaces flush errors.
	defer f.Close()
	_, err = f.Write(kubeconfigBytes)
	if err != nil {
		return "", err
	}
	err = f.Chmod(config.FileModeFile)
	if err != nil {
		return "", err
	}
	err = f.Close()
	if err != nil {
		return "", err
	}
	return f.Name(), nil
}

// InitFactoryByPath creates a kubectl Factory from an explicit kubeconfig file path and namespace.
func InitFactoryByPath(kubeconfig string, ns string) cmdutil.Factory {
	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = ptr.To(kubeconfig)
	configFlags.Namespace = ptr.To(ns)
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	return cmdutil.NewFactory(matchVersionFlags)
}

// byteRESTClientGetter is an in-memory genericclioptions.RESTClientGetter backed
// by kubeconfig bytes, letting a kubectl Factory be built without materializing a
// temp kubeconfig file. Used by InitFactoryByBytes.
type byteRESTClientGetter struct {
	clientConfig clientcmd.ClientConfig
	// loadErr is surfaced lazily from ToRESTConfig (the first method InitKubeClient
	// calls) so InitFactoryByBytes can keep InitFactoryByPath's non-nil, lazy-
	// failure contract instead of returning a nil Factory on malformed bytes.
	loadErr error
}

func (g *byteRESTClientGetter) ToRESTConfig() (*rest.Config, error) {
	if g.loadErr != nil {
		return nil, g.loadErr
	}
	return g.clientConfig.ClientConfig()
}

func (g *byteRESTClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return g.clientConfig
}

func (g *byteRESTClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	cfg, err := g.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(dc), nil
}

func (g *byteRESTClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(dc), nil
}

// InitFactoryByBytes creates a kubectl Factory directly from kubeconfig bytes and
// a namespace, without writing a temp kubeconfig file. Prefer this over
// InitFactoryByPath whenever the resulting Factory is consumed in-process only
// (i.e. NOT passed to a child process, container volume mount, or KUBECONFIG env
// var — those still need a real file). Avoiding the temp file removes a whole
// class of cross-daemon collision / permission / cleanup bugs.
func InitFactoryByBytes(kubeconfigBytes []byte, ns string) cmdutil.Factory {
	cfg, loadErr := clientcmd.Load(kubeconfigBytes)
	if cfg == nil {
		cfg = api.NewConfig()
	}
	overrides := &clientcmd.ConfigOverrides{}
	if ns != "" {
		overrides.Context.Namespace = ns
	}
	getter := &byteRESTClientGetter{
		clientConfig: clientcmd.NewDefaultClientConfig(*cfg, overrides),
		loadErr:      loadErr,
	}
	return cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(getter))
}

// GetKubeconfigCluster returns the cluster name from the current context of the factory's kubeconfig.
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

// GetKubeconfigPath flattens the factory's kubeconfig and writes it to a temp file, returning the path.
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

	return ConvertToTempKubeconfigFile(kubeconfigJsonBytes, "")
}

// GetNsForListPodAndSvc finds the first accessible namespace from nsList for listing pods and services.
func GetNsForListPodAndSvc(ctx context.Context, clientset kubernetes.Interface, nsList []string) (podNs string, svcNs string, err error) {
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
		err = fmt.Errorf("cannot list pod to add it to route table: %w", err)
		return
	}
	if podNs == "" {
		plog.G(ctx).Debugf("List all namespace pods")
	} else {
		plog.G(ctx).Debugf("List namespace %s pods", podNs)
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
		err = fmt.Errorf("cannot list service to add it to route table: %w", err)
		return
	}
	if svcNs == "" {
		plog.G(ctx).Debugf("List all namespace services")
	} else {
		plog.G(ctx).Debugf("List namespace %s services", svcNs)
	}
	return
}
