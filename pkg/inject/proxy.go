package inject

import (
	"context"
	"encoding/json"
	errors2 "errors"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/pointer"

	util2 "github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func InjectVPNSidecar(ctx1 context.Context, factory util.Factory, namespace, workload string, c util2.PodRouteConfig) error {
	object, err := util2.GetUnstructuredObject(factory, namespace, workload)
	if err != nil {
		return err
	}

	u := object.Object.(*unstructured.Unstructured)

	podTempSpec, path, err := util2.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	clientset, err := factory.KubernetesClientSet()
	if err != nil {
		return err
	}
	nodeID := fmt.Sprintf("%s.%s", object.Mapping.Resource.GroupResource().String(), object.Name)
	var ports []v1.ContainerPort
	for _, container := range podTempSpec.Spec.Containers {
		ports = append(ports, container.Ports...)
	}
	var portmap = make(map[int32]int32)
	for _, port := range ports {
		portmap[port.ContainerPort] = port.ContainerPort
	}
	err = addEnvoyConfig(clientset.CoreV1().ConfigMaps(namespace), nodeID, c, nil, ports, portmap)
	if err != nil {
		log.Errorf("Failed to add envoy config: %v", err)
		return err
	}

	AddContainer(&podTempSpec.Spec, c)

	helper := resource.NewHelper(object.Client, object.Mapping)
	// pods without controller
	if len(path) == 0 {
		log.Infof("Workload %s/%s is not controlled by any controller", namespace, workload)
		p := &v1.Pod{ObjectMeta: podTempSpec.ObjectMeta, Spec: podTempSpec.Spec}
		CleanupUselessInfo(p)
		if err = CreateAfterDeletePod(factory, p, helper); err != nil {
			return err
		}
	} else
	// controllers
	{
		log.Debugf("The %s is under controller management", workload)
		p := []P{
			{
				Op:    "replace",
				Path:  "/" + strings.Join(append(path, "spec"), "/"),
				Value: podTempSpec.Spec,
			},
		}
		marshal, _ := json.Marshal(append(p))
		_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, marshal, &v12.PatchOptions{})
		if err != nil {
			log.Errorf("Failed to inject proxy container: %v, exiting...", err)
			return err
		}
	}
	err = util2.RolloutStatus(ctx1, factory, namespace, workload, time.Minute*60)
	return err
}

func CreateAfterDeletePod(factory util.Factory, p *v1.Pod, helper *resource.Helper) error {
	_, err := helper.DeleteWithOptions(p.Namespace, p.Name, &v12.DeleteOptions{
		GracePeriodSeconds: pointer.Int64(0),
	})
	if err != nil {
		log.Errorf("Failed to delete resource: %s %s, ignore, err: %v", p.Namespace, p.Name, err)
	}
	err = retry.OnError(wait.Backoff{
		Steps:    10,
		Duration: 50 * time.Millisecond,
		Factor:   5.0,
		Jitter:   1,
	}, func(err error) bool {
		if !errors.IsAlreadyExists(err) {
			return true
		}
		clientset, err := factory.KubernetesClientSet()
		get, err := clientset.CoreV1().Pods(p.Namespace).Get(context.Background(), p.Name, v12.GetOptions{})
		if err != nil || get.Status.Phase != v1.PodRunning {
			return true
		}
		return false
	}, func() error {
		if _, err := helper.Create(p.Namespace, true, p); err != nil {
			return err
		}
		return errors2.New("")
	})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		log.Errorf("Failed to create resource: %s %s, err: %v", p.Namespace, p.Name, err)
		return err
	}
	return nil
}

func removeInboundContainer(factory util.Factory, namespace, workloads string) error {
	object, err := util2.GetUnstructuredObject(factory, namespace, workloads)
	if err != nil {
		return err
	}

	u := object.Object.(*unstructured.Unstructured)

	podTempSpec, path, err := util2.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	helper := resource.NewHelper(object.Client, object.Mapping)

	// pods
	if len(path) == 0 {
		_, err = helper.DeleteWithOptions(object.Namespace, object.Name, &v12.DeleteOptions{
			GracePeriodSeconds: pointer.Int64(0),
		})
		if err != nil {
			return err
		}
	}
	// how to scale to one
	RemoveContainer(&podTempSpec.Spec)

	bytes, err := json.Marshal([]struct {
		Op    string      `json:"op"`
		Path  string      `json:"path"`
		Value interface{} `json:"value"`
	}{{
		Op:    "replace",
		Path:  "/" + strings.Join(append(path, "spec"), "/"),
		Value: podTempSpec.Spec,
	}})
	if err != nil {
		return err
	}
	_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &v12.PatchOptions{
		//Force: &t,
	})
	return err
}

func CleanupUselessInfo(pod *v1.Pod) {
	pod.SetSelfLink("")
	pod.SetGeneration(0)
	pod.SetResourceVersion("")
	pod.SetUID("")
	pod.SetDeletionTimestamp(nil)
	pod.SetSelfLink("")
	pod.SetManagedFields(nil)
	pod.SetOwnerReferences(nil)
}

type P struct {
	Op    string      `json:"op,omitempty"`
	Path  string      `json:"path,omitempty"`
	Value interface{} `json:"value,omitempty"`
}
