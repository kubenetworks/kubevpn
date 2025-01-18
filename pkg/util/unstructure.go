package util

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	v2 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
)

func GetUnstructuredObject(f util.Factory, ns string, workloads string) (*resource.Info, error) {
	do := f.NewBuilder().
		Unstructured().
		NamespaceParam(ns).DefaultNamespace().AllNamespaces(false).
		ResourceTypeOrNameArgs(true, workloads).
		ContinueOnError().
		Latest().
		Flatten().
		TransformRequests(func(req *rest.Request) { req.Param("includeObject", "Object") }).
		Do()
	if err := do.Err(); err != nil {
		return nil, err
	}
	infos, err := do.Infos()
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 {
		return nil, fmt.Errorf("not found workloads %s", workloads)
	}
	return infos[0], err
}

func GetUnstructuredObjectList(f util.Factory, ns string, workloads []string) ([]*resource.Info, error) {
	do := f.NewBuilder().
		Unstructured().
		NamespaceParam(ns).DefaultNamespace().AllNamespaces(false).
		ResourceTypeOrNameArgs(true, workloads...).
		ContinueOnError().
		Latest().
		Flatten().
		TransformRequests(func(req *rest.Request) { req.Param("includeObject", "Object") }).
		Do()
	if err := do.Err(); err != nil {
		return nil, err
	}
	infos, err := do.Infos()
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 {
		return nil, errors.New(fmt.Sprintf("Not found resource %v", workloads))
	}
	return infos, err
}

func GetUnstructuredObjectBySelector(f util.Factory, ns string, selector string) ([]*resource.Info, error) {
	do := f.NewBuilder().
		Unstructured().
		NamespaceParam(ns).DefaultNamespace().AllNamespaces(false).
		ResourceTypeOrNameArgs(true, "all").
		LabelSelector(selector).
		ContinueOnError().
		Latest().
		Flatten().
		TransformRequests(func(req *rest.Request) { req.Param("includeObject", "Object") }).
		Do()
	if err := do.Err(); err != nil {
		return nil, err
	}
	infos, err := do.Infos()
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 {
		return nil, errors.New("Not found")
	}
	return infos, err
}

func GetPodTemplateSpecPath(u *unstructured.Unstructured) (*v1.PodTemplateSpec, []string, error) {
	var stringMap map[string]interface{}
	var b bool
	var err error
	var path []string
	if stringMap, b, err = unstructured.NestedMap(u.Object, "spec", "template"); b && err == nil {
		path = []string{"spec", "template"}
	} else if stringMap, b, err = unstructured.NestedMap(u.Object); b && err == nil {
		path = []string{}
	} else {
		return nil, nil, err
	}
	marshal, err := json.Marshal(stringMap)
	if err != nil {
		return nil, nil, err
	}
	var p v1.PodTemplateSpec
	if err = json.Unmarshal(marshal, &p); err != nil {
		return nil, nil, err
	}
	return &p, path, nil
}

func GetAnnotation(f util.Factory, ns string, resources string) (map[string]string, error) {
	ownerReference, err := GetTopOwnerReference(f, ns, resources)
	if err != nil {
		return nil, err
	}
	u, ok := ownerReference.Object.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("can not convert to unstaructed")
	}
	annotations := u.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	return annotations, nil
}

/*
NormalizedResource convert user parameter to standard, example:

	pod/productpage-7667dfcddb-cbsn5 --> deployments.apps/productpage
	service/productpage --> deployments.apps/productpage
	replicaset/productpage-7667dfcddb --> deployments.apps/productpage
	deployment: productpage --> deployments.apps/productpage

	pods without controller
	pod/productpage-without-controller --> pod/productpage-without-controller
	service/productpage-without-pod --> controller/controllerName
*/
func NormalizedResource(ctx context.Context, f util.Factory, clientset *kubernetes.Clientset, ns string, workloads []string) ([]string, error) {
	if len(workloads) == 0 {
		return nil, nil
	}

	objectList, err := GetUnstructuredObjectList(f, ns, workloads)
	if err != nil {
		return nil, err
	}
	var resources []string
	for _, info := range objectList {
		resources = append(resources, fmt.Sprintf("%s/%s", info.Mapping.Resource.GroupResource().String(), info.Name))
	}
	workloads = resources

	// normal workloads, like pod with controller, deployments, statefulset, replicaset etc...
	for i, workload := range workloads {
		var ownerReference *resource.Info
		ownerReference, err = GetTopOwnerReference(f, ns, workload)
		if err == nil {
			workloads[i] = fmt.Sprintf("%s/%s", ownerReference.Mapping.Resource.GroupResource().String(), ownerReference.Name)
		}
	}
	// service which associate with pod
	for i, workload := range workloads {
		var object *resource.Info
		object, err = GetUnstructuredObject(f, ns, workload)
		if err != nil {
			return nil, err
		}
		if object.Mapping.Resource.Resource != "services" {
			continue
		}
		var svc *v1.Service
		svc, err = clientset.CoreV1().Services(ns).Get(ctx, object.Name, v2.GetOptions{})
		if err != nil {
			continue
		}
		var selector labels.Selector
		_, selector, err = polymorphichelpers.SelectorsForObject(svc)
		if err != nil {
			continue
		}
		var podList *v1.PodList
		podList, err = clientset.CoreV1().Pods(ns).List(ctx, v2.ListOptions{LabelSelector: selector.String()})
		// if pod is not empty, using pods to find top controller
		if err == nil && podList != nil && len(podList.Items) != 0 {
			var ownerReference *resource.Info
			ownerReference, err = GetTopOwnerReference(f, ns, fmt.Sprintf("%s/%s", "pods", podList.Items[0].Name))
			if err == nil {
				workloads[i] = fmt.Sprintf("%s/%s", ownerReference.Mapping.Resource.GroupResource().String(), ownerReference.Name)
			}
		} else { // if list is empty, means not create pods, just controllers
			var controller sets.Set[string]
			controller, err = GetTopOwnerReferenceBySelector(f, ns, selector.String())
			if err == nil {
				if len(controller) > 0 {
					workloads[i] = controller.UnsortedList()[0]
				}
			}
			// only a single service, not support it yet
			if controller == nil || controller.Len() == 0 {
				return nil, fmt.Errorf("not support resources: %s", workload)
			}
		}
	}
	return workloads, nil
}
