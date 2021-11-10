package remote

import (
	"context"
	"errors"
	"fmt"
	"github.com/wencaiwulue/kubevpn/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

type ServiceController struct {
	factory   cmdutil.Factory
	clientset *kubernetes.Clientset
	namespace string
	name      string
	f         func() error
}

func NewServiceController(factory cmdutil.Factory, clientset *kubernetes.Clientset, namespace, name string) *ServiceController {
	return &ServiceController{
		factory:   factory,
		clientset: clientset,
		namespace: namespace,
		name:      name,
	}
}

func (s *ServiceController) Inject() (map[string]string, *v1.PodSpec, error) {
	object, err := util.GetUnstructuredObject(s.factory, s.namespace, fmt.Sprintf("services/%s", s.name))
	if err != nil {
		return nil, nil, err
	}
	asSelector, _ := metav1.LabelSelectorAsSelector(util.GetLabelSelector(object.Object))
	podList, _ := s.clientset.CoreV1().Pods(s.namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: asSelector.String(),
	})
	if len(podList.Items) == 0 {
		return nil, nil, errors.New("this should not happened")
	}
	// if podList is not one, needs to merge ???
	podController := NewPodController(s.factory, s.clientset, s.namespace, "pods", podList.Items[0].Name)

	labels, zero, err := podController.Inject()
	s.f = podController.f
	return labels, zero, err
}

func (s *ServiceController) Cancel() error {
	return s.f()
}
