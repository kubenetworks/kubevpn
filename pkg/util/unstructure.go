package util

import (
	"encoding/json"
	"fmt"

	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/cmd/util"
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
