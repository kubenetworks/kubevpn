package inject

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TLSEnvVars propagates the manager tunnel identity to the node-agent DaemonSet.
func TLSEnvVars(secret *corev1.Secret) []corev1.EnvVar {
	vars := []corev1.EnvVar{
		{Name: config.TLSServerName, Value: string(secret.Data[config.TLSServerName])},
		{Name: config.TLSCertKey, Value: string(secret.Data[config.TLSCertKey])},
		{Name: config.TLSPrivateKeyKey, Value: string(secret.Data[config.TLSPrivateKeyKey])},
	}
	if value, ok := secret.Data[config.ObfuscationKey]; ok {
		vars = append(vars, corev1.EnvVar{Name: config.ObfuscationKey, Value: string(value)})
	}
	return vars
}
