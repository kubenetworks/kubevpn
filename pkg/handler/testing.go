package handler

import (
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
)

// NewConnectOptionsForTest creates a ConnectOptions with an initialized factory
// for use in unit tests that need buildConnectionStatus or other factory-dependent code paths.
func NewConnectOptionsForTest(factory cmdutil.Factory, kubeconfigPath, namespace string) *ConnectOptions {
	return &ConnectOptions{
		factory:              factory,
		OriginKubeconfigPath: kubeconfigPath,
		OriginNamespace:      namespace,
	}
}

// NewConnectOptionsWithIDForTest creates a ConnectOptions with a preset connectionID
// for unit tests that need GetConnectionID() to return a known value.
func NewConnectOptionsWithIDForTest(connectionID string) *ConnectOptions {
	return &ConnectOptions{
		dhcp: dhcp.NewManagerForTest(connectionID),
	}
}
