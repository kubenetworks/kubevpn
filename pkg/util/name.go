package util

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// Join concatenates names with underscores as separators.
func Join(names ...string) string {
	return strings.Join(names, "_")
}

// ContainerNet returns the Docker network mode string "container:<name>" for sharing a container's network namespace.
func ContainerNet(name string) string {
	return fmt.Sprintf("container:%s", name)
}

// GenEnvoyUID generates a unique Envoy identifier by joining namespace and uid with a dot.
func GenEnvoyUID(ns, uid string) string {
	return fmt.Sprintf("%s.%s", ns, uid)
}

// GenKubeconfigTempPath generates a temp file path for kubeconfig based on the cluster name, namespace, and timestamp.
func GenKubeconfigTempPath(kubeconfigBytes []byte) string {
	var path string
	cluster, ns, _ := GetCluster(kubeconfigBytes)
	if !ContainsPathSeparator(cluster) && !ContainsPathSeparator(ns) {
		pattern := fmt.Sprintf("%s_%s_%d", cluster, ns, time.Now().Unix())
		pattern = strings.ReplaceAll(pattern, string(os.PathSeparator), "-")
		path = filepath.Join(config.GetTempPath(), pattern)
	} else {
		path = filepath.Join(config.GetTempPath(), fmt.Sprintf("%d", time.Now().Unix()))
	}
	return path
}

// ContainsPathSeparator reports whether the pattern contains any OS path separator characters.
func ContainsPathSeparator(pattern string) bool {
	return strings.ContainsRune(pattern, os.PathSeparator)
}

// GetCluster extracts the cluster name and namespace from kubeconfig bytes.
func GetCluster(kubeConfigBytes []byte) (cluster string, ns string, err error) {
	var clientConfig clientcmd.ClientConfig
	clientConfig, err = clientcmd.NewClientConfigFromBytes(kubeConfigBytes)
	if err != nil {
		return
	}
	var rawConfig api.Config
	rawConfig, err = clientConfig.RawConfig()
	if err != nil {
		return
	}
	if err = api.FlattenConfig(&rawConfig); err != nil {
		return
	}
	if rawConfig.Contexts == nil {
		return
	}
	kubeContext := rawConfig.Contexts[rawConfig.CurrentContext]
	if kubeContext == nil {
		return
	}
	cluster = kubeContext.Cluster
	ns = kubeContext.Namespace
	return
}
