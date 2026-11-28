package xds

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

func validateDesiredVirtualConfig(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read desired virtual config: %w", err)
	}
	var virtuals []*controlplane.Virtual
	if data := cm.Data[config.KeyEnvoy]; data != "" {
		if err := yaml.Unmarshal([]byte(data), &virtuals); err != nil {
			return fmt.Errorf("parse desired virtual config: %w", err)
		}
	}
	return controlplane.ValidateDesiredVirtuals(virtuals)
}
