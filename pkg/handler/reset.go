package handler

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/yaml"

	clusterworkload "github.com/wencaiwulue/kubevpn/v2/pkg/cluster/workload"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Reset removes node-agent proxy state without modifying workloads or Services.
func (c *ConnectOptions) Reset(ctx context.Context, namespace string, workloads []string) error {
	if c == nil || c.clientset == nil {
		return nil
	}

	var err error
	workloads, _, err = clusterworkload.NormalizedResource(c.factory, namespace, workloads)
	if err != nil {
		return err
	}

	plog.StepStart(ctx, "Resetting workloads")
	err = resetConfigMap(ctx, c.clientset.CoreV1().ConfigMaps(c.ManagerNamespace), namespace, workloads)
	if err != nil {
		plog.G(ctx).Error(err)
		return err
	}
	plog.StepDone(ctx, "Reset %d workloads", len(workloads))

	return nil
}

func resetConfigMap(ctx context.Context, mapInterface v1.ConfigMapInterface, namespace string, workloads []string) error {
	cm, err := mapInterface.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if cm == nil || cm.Data == nil || len(cm.Data[config.KeyEnvoy]) == 0 {
		plog.G(ctx).Infof("No proxy resources found")
		return nil
	}
	v := make([]*controlplane.Virtual, 0)
	str := cm.Data[config.KeyEnvoy]
	if err = yaml.Unmarshal([]byte(str), &v); err != nil {
		plog.G(ctx).Errorf("Unmarshal envoy config error: %v", err)
		return nil
	}
	if err = controlplane.ValidateDesiredVirtuals(v); err != nil {
		return err
	}
	ws := sets.New[string]()
	for _, workload := range workloads {
		ws.Insert(util.ConvertWorkloadToUID(workload))
	}

	for i := 0; i < len(v); i++ {
		if ws.Has(v[i].UID) && v[i].Namespace == namespace {
			if isLegacySidecarVirtual(v[i]) {
				return fmt.Errorf("legacy sidecar-injected workload %s is not supported", v[i].UID)
			}
			v = append(v[:i], v[i+1:]...)
			i--
		}
	}

	marshal, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("failed to marshal envoy config: %w", err)
	}
	cm.Data[config.KeyEnvoy] = string(marshal)
	_, err = mapInterface.Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update configmap %s: %w: %w", config.ConfigMapPodTrafficManager, err, config.ErrCleanupFailed)
	}
	return nil
}

func isLegacySidecarVirtual(virtual *controlplane.Virtual) bool {
	if virtual.Selector != "" || len(virtual.AgentOrigins) != 0 {
		return false
	}
	for _, rule := range virtual.Rules {
		if !rule.IsAgent {
			return true
		}
	}
	return false
}
