package handler

import (
	"context"
	"encoding/json"
	"strings"

	log "github.com/sirupsen/logrus"
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
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Reset
// 1) reset configmap
// 2) remove inject containers envoy and vpn
// 3) restore service targetPort to containerPort
func (c *ConnectOptions) Reset(ctx context.Context, workloads []string) error {
	if c == nil || c.clientset == nil {
		return nil
	}

	var err error
	workloads, err = util.NormalizedResource(ctx, c.factory, c.clientset, c.Namespace, workloads)
	if err != nil {
		return err
	}

	err = resetConfigMap(ctx, c.clientset.CoreV1().ConfigMaps(c.Namespace), workloads)
	if err != nil {
		log.Error(err)
	}

	for _, workload := range workloads {
		err = removeInjectContainer(ctx, c.factory, c.clientset, c.Namespace, workload)
		if err != nil {
			log.Error(err)
		}
	}

	return nil
}

func resetConfigMap(ctx context.Context, mapInterface v1.ConfigMapInterface, workloads []string) error {
	cm, err := mapInterface.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if cm == nil || cm.Data == nil || len(cm.Data[config.KeyEnvoy]) == 0 {
		log.Infof("No proxy resources found")
		return nil
	}
	var v = make([]*controlplane.Virtual, 0)
	str := cm.Data[config.KeyEnvoy]
	if err = yaml.Unmarshal([]byte(str), &v); err != nil {
		log.Errorf("Unmarshal envoy config error: %v", err)
		return nil
	}
	ws := sets.New[string]()
	for _, workload := range workloads {
		ws.Insert(util.ConvertWorkloadToUid(workload))
	}

	for i := 0; i < len(v); i++ {
		if ws.Has(v[i].Uid) {
			v = append(v[:i], v[i+1:]...)
			i--
		}
	}

	marshal, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	cm.Data[config.KeyEnvoy] = string(marshal)
	_, err = mapInterface.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func removeInjectContainer(ctx context.Context, factory cmdutil.Factory, clientset *kubernetes.Clientset, namespace, workload string) error {
	object, err := util.GetUnstructuredObject(factory, namespace, workload)
	if err != nil {
		log.Errorf("Failed to get unstructured object: %v", err)
		return err
	}

	u := object.Object.(*unstructured.Unstructured)
	templateSpec, depth, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		log.Errorf("Failed to get template spec path: %v", err)
		return err
	}

	log.Infof("Leaving workload %s", workload)

	inject.RemoveContainers(templateSpec)

	helper := pkgresource.NewHelper(object.Client, object.Mapping)
	log.Debugf("The %s is under controller management", workload)
	// resource with controller, like deployment,statefulset
	var bytes []byte
	bytes, err = json.Marshal([]inject.P{
		{
			Op:    "replace",
			Path:  "/" + strings.Join(append(depth, "spec"), "/"),
			Value: templateSpec.Spec,
		},
	})
	if err != nil {
		log.Errorf("Failed to generate json patch: %v", err)
		return err
	}
	_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{})
	if err != nil {
		log.Errorf("Failed to patch resource: %s %s: %v", object.Mapping.Resource.Resource, object.Name, err)
		return err
	}

	var portmap = make(map[int32]int32)
	for _, container := range templateSpec.Spec.Containers {
		for _, port := range container.Ports {
			portmap[port.ContainerPort] = port.ContainerPort
		}
	}
	err = inject.ModifyServiceTargetPort(ctx, clientset, namespace, templateSpec.Labels, portmap)
	return err
}
