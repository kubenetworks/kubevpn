package util

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	clientcmdlatest "k8s.io/client-go/tools/clientcmd/api/latest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

func GetKubeConfigPath(f cmdutil.Factory) string {
	rawConfig := f.ToRawKubeConfigLoader()
	if rawConfig.ConfigAccess().IsExplicitFile() {
		return rawConfig.ConfigAccess().GetExplicitFile()
	} else {
		return rawConfig.ConfigAccess().GetDefaultFilename()
	}
}

func ConvertK8sApiServerToDomain(kubeConfigPath string) (newPath string, err error) {
	var kubeConfigBytes []byte
	kubeConfigBytes, err = os.ReadFile(kubeConfigPath)
	if err != nil {
		return
	}
	var config clientcmd.ClientConfig
	config, err = clientcmd.NewClientConfigFromBytes(kubeConfigBytes)
	if err != nil {
		return
	}
	var rawConfig api.Config
	rawConfig, err = config.RawConfig()
	if err != nil {
		return
	}
	if err = api.FlattenConfig(&rawConfig); err != nil {
		return
	}
	if rawConfig.Contexts == nil {
		err = errors.New("kubeconfig is invalid")
		return
	}
	kubeContext := rawConfig.Contexts[rawConfig.CurrentContext]
	if kubeContext == nil {
		err = errors.New("kubeconfig is invalid")
		return
	}
	cluster := rawConfig.Clusters[kubeContext.Cluster]
	if cluster == nil {
		err = errors.New("kubeconfig is invalid")
		return
	}
	var u *url.URL
	u, err = url.Parse(cluster.Server)
	if err != nil {
		return
	}
	var remote netip.AddrPort
	remote, err = netip.ParseAddrPort(u.Host)
	if err != nil {
		return
	}
	host := fmt.Sprintf("%s://%s", u.Scheme, net.JoinHostPort("kubernetes", strconv.Itoa(int(remote.Port()))))
	rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].Server = host
	rawConfig.SetGroupVersionKind(schema.GroupVersionKind{Version: latest.Version, Kind: "Config"})

	var convertedObj runtime.Object
	convertedObj, err = latest.Scheme.ConvertToVersion(&rawConfig, latest.ExternalVersion)
	if err != nil {
		return
	}
	var marshal []byte
	marshal, err = json.Marshal(convertedObj)
	if err != nil {
		return
	}
	newPath, err = ConvertToTempKubeconfigFile(marshal, "")
	if err != nil {
		return
	}
	return
}

func ConvertConfig(factory cmdutil.Factory) ([]byte, error) {
	rawConfig, err := factory.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return nil, err
	}
	err = api.FlattenConfig(&rawConfig)
	if err != nil {
		return nil, err
	}
	rawConfig.SetGroupVersionKind(schema.GroupVersionKind{Version: clientcmdlatest.Version, Kind: "Config"})
	var convertedObj runtime.Object
	convertedObj, err = latest.Scheme.ConvertToVersion(&rawConfig, latest.ExternalVersion)
	if err != nil {
		return nil, err
	}
	return json.Marshal(convertedObj)
}
