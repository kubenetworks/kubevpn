package handler

import (
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

// NewConnectOptionsForTest creates a ConnectOptions with an initialized factory
// for use in unit tests that need buildConnectionStatus or other factory-dependent code paths.
func NewConnectOptionsForTest(factory cmdutil.Factory, kubeconfigPath, namespace string) *ConnectOptions {
	return &ConnectOptions{
		K8sClient:            K8sClient{factory: factory},
		OriginKubeconfigPath: kubeconfigPath,
		WorkloadNamespace:    namespace,
	}
}
