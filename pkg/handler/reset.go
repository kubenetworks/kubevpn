package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	pkgresource "k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	"github.com/wencaiwulue/kubevpn/v2/pkg/xds"
)

// Reset removes injected sidecar containers and restores workloads to their original spec.
func (c *ConnectOptions) Reset(ctx context.Context, namespace string, workloads []string) error {
	if c == nil || c.clientset == nil {
		return nil
	}

	var err error
	workloads, _, err = util.NormalizedResource(c.factory, namespace, workloads)
	if err != nil {
		return err
	}

	plog.StepStart(ctx, "Resetting workloads")
	err = resetConfigMap(ctx, c.clientset.CoreV1().ConfigMaps(c.ManagerNamespace), namespace, workloads)
	if err != nil {
		plog.G(ctx).Error(err)
	}

	for _, workload := range workloads {
		err = removeInjectContainer(ctx, c.factory, c.clientset, namespace, workload)
		if err != nil {
			plog.G(ctx).Error(err)
		}
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
	v := make([]*xds.Virtual, 0)
	str := cm.Data[config.KeyEnvoy]
	if err = yaml.Unmarshal([]byte(str), &v); err != nil {
		plog.G(ctx).Errorf("Unmarshal envoy config error: %v", err)
		return nil
	}
	ws := sets.New[string]()
	for _, workload := range workloads {
		ws.Insert(util.ConvertWorkloadToUID(workload))
	}

	for i := 0; i < len(v); i++ {
		if ws.Has(v[i].UID) && v[i].Namespace == namespace {
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

func removeInjectContainer(ctx context.Context, factory cmdutil.Factory, clientset kubernetes.Interface, namespace, workload string) error {
	object, controller, err := util.GetTopOwnerObject(ctx, factory, namespace, workload)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get unstructured object: %v", err)
		return err
	}

	u := controller.Object.(*unstructured.Unstructured)
	templateSpec, depth, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get template spec path: %v", err)
		return err
	}

	plog.G(ctx).Debugf("Resetting workload %q", workload)

	inject.RemoveContainers(&templateSpec.Spec)

	helper := pkgresource.NewHelper(controller.Client, controller.Mapping)
	plog.G(ctx).Debugf("The %s is under controller management", workload)
	// resource with controller, like deployment,statefulset
	var bytes []byte
	bytes, err = json.Marshal([]inject.JSONPatchOp{
		{
			Op:    "replace",
			Path:  "/" + strings.Join(append(depth, "spec"), "/"),
			Value: templateSpec.Spec,
		},
	})
	if err != nil {
		plog.G(ctx).Errorf("Failed to generate json patch: %v", err)
		return err
	}
	_, err = helper.Patch(controller.Namespace, controller.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Failed to patch resource: %s %s: %v", controller.Mapping.Resource.Resource, controller.Name, err)
		return err
	}
	workloadRef := fmt.Sprintf("%s/%s", controller.Mapping.Resource.Resource, controller.Name)
	if err = util.RolloutStatus(ctx, factory, controller.Namespace, workloadRef); err != nil {
		plog.G(ctx).Warnf("Rollout status check failed for %s: %v", workloadRef, err)
	}
	if !util.IsK8sService(object) {
		return nil
	}

	if err = inject.RestoreServiceTargetPort(ctx, clientset, namespace, object.Name); err != nil {
		return fmt.Errorf("failed to restore service %s target ports: %w: %w", object.Name, err, config.ErrCleanupFailed)
	}
	return nil
}
