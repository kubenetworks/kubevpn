package remote

import (
	"context"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

type PodController struct {
	factory   cmdutil.Factory
	clientset *kubernetes.Clientset
	namespace string
	resource  string
	name      string
	f         func() error
}

func NewPodController(factory cmdutil.Factory, clientset *kubernetes.Clientset, namespace, resource, name string) *PodController {
	return &PodController{
		factory:   factory,
		clientset: clientset,
		resource:  resource,
		namespace: namespace,
		name:      name,
	}
}

func (c *PodController) Inject() (map[string]string, *v1.PodSpec, error) {
	pod, err := c.clientset.CoreV1().Pods(c.namespace).Get(context.TODO(), c.name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	topController := util.GetTopController(c.factory, c.clientset, c.namespace, fmt.Sprintf("%s/%s", c.resource, c.name))
	// controllerBy is empty
	if len(topController.Name) == 0 || len(topController.Resource) == 0 {
		c.f = func() error {
			_, err = c.clientset.CoreV1().Pods(c.namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
			if err != nil {
				log.Warnln(err)
			}
			return err
		}
		_ = c.clientset.CoreV1().Pods(c.namespace).Delete(context.TODO(), c.name, metav1.DeleteOptions{})
		return pod.Labels, &pod.Spec, nil
	}
	object, err := util.GetUnstructuredObject(c.factory, c.namespace, fmt.Sprintf("%s/%s", topController.Resource, topController.Name))
	helper := resource.NewHelper(object.Client, object.Mapping)
	c.f = func() error {
		_, err = helper.Create(c.namespace, true, object.Object)
		return err
	}
	if _, err = helper.Delete(c.namespace, object.Name); err != nil {
		return nil, nil, err
	}
	return pod.Labels, &pod.Spec, err
}

func (c *PodController) Cancel() error {
	return c.f()
}
