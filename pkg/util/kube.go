package util

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	clientcmdlatest "k8s.io/client-go/tools/clientcmd/api/latest"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	pkgconfig "github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// InitKubeClient initializes the core Kubernetes client objects from a factory.
// Used by ConnectOptions, SyncOptions, and run.Options to avoid triplicating the same setup.
func InitKubeClient(f cmdutil.Factory) (cfg *rest.Config, restclient *rest.RESTClient, clientset *kubernetes.Clientset, namespace string, err error) {
	if cfg, err = f.ToRESTConfig(); err != nil {
		return
	}
	if restclient, err = f.RESTClient(); err != nil {
		return
	}
	if clientset, err = f.KubernetesClientSet(); err != nil {
		return
	}
	namespace, _, err = f.ToRawKubeConfigLoader().Namespace()
	return
}

// GetKubeConfigPath returns the absolute path of the kubeconfig file used by the factory.
func GetKubeConfigPath(f cmdutil.Factory) string {
	rawConfig := f.ToRawKubeConfigLoader()
	if rawConfig.ConfigAccess().IsExplicitFile() {
		file := rawConfig.ConfigAccess().GetExplicitFile()
		abs, err := filepath.Abs(file)
		if err != nil {
			return file
		}
		return abs
	}
	return rawConfig.ConfigAccess().GetDefaultFilename()
}

// ConvertK8sApiServerToDomain rewrites the API server address in the kubeconfig to use the "kubernetes" hostname and returns a temp file path.
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
		err = pkgconfig.ErrInvalidKubeconfig
		return
	}
	kubeContext := rawConfig.Contexts[rawConfig.CurrentContext]
	if kubeContext == nil {
		err = pkgconfig.ErrInvalidKubeconfig
		return
	}
	cluster := rawConfig.Clusters[kubeContext.Cluster]
	if cluster == nil {
		err = pkgconfig.ErrInvalidKubeconfig
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
	var marshal []byte
	marshal, err = serializeKubeconfig(&rawConfig)
	if err != nil {
		return
	}
	newPath, err = ConvertToTempKubeconfigFile(marshal, "")
	if err != nil {
		return
	}
	return
}

// ModifyAPIServer replaces the API server address in kubeconfig bytes with newAPIServer and returns the modified bytes along with the original address.
func ModifyAPIServer(ctx context.Context, kubeconfigBytes []byte, newAPIServer netip.AddrPort) ([]byte, netip.AddrPort, error) {
	clientConfig, err := clientcmd.NewClientConfigFromBytes(kubeconfigBytes)
	if err != nil {
		return nil, netip.AddrPort{}, err
	}
	var rawConfig api.Config
	rawConfig, err = clientConfig.RawConfig()
	if err != nil {
		plog.G(ctx).WithError(err).Errorf("failed to build config: %v", err)
		return nil, netip.AddrPort{}, err
	}
	if err = api.FlattenConfig(&rawConfig); err != nil {
		plog.G(ctx).Errorf("failed to flatten config: %v", err)
		return nil, netip.AddrPort{}, err
	}
	if rawConfig.Contexts == nil {
		err = pkgconfig.ErrInvalidKubeconfig
		plog.G(ctx).Error("cannot get contexts")
		return nil, netip.AddrPort{}, err
	}
	kubeContext := rawConfig.Contexts[rawConfig.CurrentContext]
	if kubeContext == nil {
		err = pkgconfig.ErrInvalidKubeconfig
		plog.G(ctx).Errorf("cannot find kubeconfig context %s", rawConfig.CurrentContext)
		return nil, netip.AddrPort{}, err
	}
	cluster := rawConfig.Clusters[kubeContext.Cluster]
	if cluster == nil {
		err = pkgconfig.ErrInvalidKubeconfig
		plog.G(ctx).Errorf("cannot find cluster %s", kubeContext.Cluster)
		return nil, netip.AddrPort{}, err
	}
	var u *url.URL
	u, err = url.Parse(cluster.Server)
	if err != nil {
		plog.G(ctx).Errorf("failed to parse cluster url: %v", err)
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
			err = fmt.Errorf("kubeconfig server uses wrong protocol %q: %w", u.Scheme, pkgconfig.ErrKubeconfigWrongProtocol)
			plog.G(ctx).Error(err)
			return nil, netip.AddrPort{}, err
		}
	}
	ips, err := net.LookupHost(serverHost)
	if err != nil {
		return nil, netip.AddrPort{}, err
	}

	if len(ips) == 0 {
		// handle error: no IP associated with the hostname
		err = fmt.Errorf("kubeconfig: no IP associated with the hostname %s: %w", serverHost, pkgconfig.ErrKubeconfigUnresolvable)
		plog.G(ctx).Error(err)
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
	// TODO: add cli option to skip tls verify
	// rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].CertificateAuthorityData = nil
	// rawConfig.Clusters[rawConfig.Contexts[rawConfig.CurrentContext].Cluster].InsecureSkipTLSVerify = true
	marshal, err := serializeKubeconfig(&rawConfig)
	if err != nil {
		plog.G(ctx).Errorf("failed to serialize kubeconfig: %v", err)
		return nil, netip.AddrPort{}, err
	}
	return marshal, remote, nil
}

func serializeKubeconfig(cfg *api.Config) ([]byte, error) {
	cfg.SetGroupVersionKind(schema.GroupVersionKind{Version: clientcmdlatest.Version, Kind: "Config"})
	obj, err := clientcmdlatest.Scheme.ConvertToVersion(cfg, clientcmdlatest.ExternalVersion)
	if err != nil {
		return nil, err
	}
	return json.Marshal(obj)
}
