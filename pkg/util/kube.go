package util

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"

	errors2 "github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	clientcmdlatest "k8s.io/client-go/tools/clientcmd/api/latest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/log"
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

func ModifyAPIServer(ctx context.Context, kubeconfigBytes []byte, newAPIServer netip.AddrPort) ([]byte, netip.AddrPort, error) {
	clientConfig, err := clientcmd.NewClientConfigFromBytes(kubeconfigBytes)
	if err != nil {
		return nil, netip.AddrPort{}, err
	}
	var rawConfig api.Config
	rawConfig, err = clientConfig.RawConfig()
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to build config: %v", err)
		return nil, netip.AddrPort{}, err
	}
	if err = api.FlattenConfig(&rawConfig); err != nil {
		log.G(ctx).Errorf("failed to flatten config: %v", err)
		return nil, netip.AddrPort{}, err
	}
	if rawConfig.Contexts == nil {
		err = errors2.New("kubeconfig is invalid")
		log.G(ctx).Error("can not get contexts")
		return nil, netip.AddrPort{}, err
	}
	kubeContext := rawConfig.Contexts[rawConfig.CurrentContext]
	if kubeContext == nil {
		err = errors2.New("kubeconfig is invalid")
		log.G(ctx).Errorf("can not find kubeconfig context %s", rawConfig.CurrentContext)
		return nil, netip.AddrPort{}, err
	}
	cluster := rawConfig.Clusters[kubeContext.Cluster]
	if cluster == nil {
		err = errors2.New("kubeconfig is invalid")
		log.G(ctx).Errorf("can not find cluster %s", kubeContext.Cluster)
		return nil, netip.AddrPort{}, err
	}
	var u *url.URL
	u, err = url.Parse(cluster.Server)
	if err != nil {
		log.G(ctx).Errorf("failed to parse cluster url: %v", err)
		return nil, netip.AddrPort{}, err
	}

	serverHost := u.Hostname()
	serverPort := u.Port()
	if serverPort == "" {
		if u.Scheme == "https" {
			serverPort = "443"
		} else if u.Scheme == "http" {
			serverPort = "80"
		} else {
			// handle other schemes if necessary
			err = errors2.New("kubeconfig is invalid: wrong protocol")
			log.G(ctx).Error(err)
			return nil, netip.AddrPort{}, err
		}
	}
	ips, err := net.LookupHost(serverHost)
	if err != nil {
		return nil, netip.AddrPort{}, err
	}

	if len(ips) == 0 {
		// handle error: no IP associated with the hostname
		err = fmt.Errorf("kubeconfig: no IP associated with the hostname %s", serverHost)
		log.G(ctx).Error(err)
		return nil, netip.AddrPort{}, err
	}

	var remote netip.AddrPort
	// Use the first IP address
	remote, err = netip.ParseAddrPort(net.JoinHostPort(ips[0], serverPort))
	if err != nil {
		return nil, netip.AddrPort{}, err
	}

	rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].Server = fmt.Sprintf("%s://%s", u.Scheme, newAPIServer.String())
	rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].TLSServerName = serverHost
	// To Do: add cli option to skip tls verify
	// rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].CertificateAuthorityData = nil
	// rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].InsecureSkipTLSVerify = true
	rawConfig.SetGroupVersionKind(schema.GroupVersionKind{Version: clientcmdlatest.Version, Kind: "Config"})

	var convertedObj runtime.Object
	convertedObj, err = clientcmdlatest.Scheme.ConvertToVersion(&rawConfig, clientcmdlatest.ExternalVersion)
	if err != nil {
		log.G(ctx).Errorf("failed to build config: %v", err)
		return nil, netip.AddrPort{}, err
	}
	var marshal []byte
	marshal, err = json.Marshal(convertedObj)
	if err != nil {
		log.G(ctx).Errorf("failed to marshal config: %v", err)
		return nil, netip.AddrPort{}, err
	}
	return marshal, remote, nil
}
