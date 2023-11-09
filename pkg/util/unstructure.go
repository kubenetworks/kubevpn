package util

import (
	"encoding/json"

	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

func GetUnstructuredObject(f util.Factory, namespace string, workloads string) (*resource.Info, error) {
	do := f.NewBuilder().
		Unstructured().
		NamespaceParam(namespace).DefaultNamespace().AllNamespaces(false).
		ResourceTypeOrNameArgs(true, workloads).
		ContinueOnError().
		Latest().
		Flatten().
		TransformRequests(func(req *rest.Request) { req.Param("includeObject", "Object") }).
		Do()
	if err := do.Err(); err != nil {
		logrus.Warn(err)
		return nil, err
	}
	infos, err := do.Infos()
	if err != nil {
		logrus.Println(err)
		return nil, err
	}
	if len(infos) == 0 {
		return nil, errors.Errorf("not found workloads %s", workloads)
	}
	return infos[0], err
}

func GetUnstructuredObjectList(f util.Factory, namespace string, workloads []string) ([]*resource.Info, error) {
	do := f.NewBuilder().
		Unstructured().
		NamespaceParam(namespace).DefaultNamespace().AllNamespaces(false).
		ResourceTypeOrNameArgs(true, workloads...).
		ContinueOnError().
		Latest().
		Flatten().
		TransformRequests(func(req *rest.Request) { req.Param("includeObject", "Object") }).
		Do()
	if err := do.Err(); err != nil {
		logrus.Warn(err)
		return nil, err
	}
	infos, err := do.Infos()
	if err != nil {
		err = errors.Wrap(err, "Error occurred while getting information")
		return nil, err
	}
	if len(infos) == 0 {
		return nil, errors.Errorf("not found resource %v", workloads)
	}
	return infos, err
}

func GetUnstructuredObjectBySelector(f util.Factory, namespace string, selector string) ([]*resource.Info, error) {
	do := f.NewBuilder().
		Unstructured().
		NamespaceParam(namespace).DefaultNamespace().AllNamespaces(false).
		ResourceTypeOrNameArgs(true, "all").
		LabelSelector(selector).
		ContinueOnError().
		Latest().
		Flatten().
		TransformRequests(func(req *rest.Request) { req.Param("includeObject", "Object") }).
		Do()
	if err := do.Err(); err != nil {
		logrus.Warn(err)
		return nil, err
	}
	infos, err := do.Infos()
	if err != nil {
		logrus.Println(err)
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
		err = errors.Wrap(err, "Error occurred while marshalling JSON")
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
		err = errors.Wrap(err, "Error occurred while getting top owner reference")
		return nil, err
	}
	u, ok := ownerReference.Object.(*unstructured.Unstructured)
	if !ok {
		return nil, errors.Errorf("can not convert to unstaructed")
	}
	annotations := u.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	return annotations, nil
}
