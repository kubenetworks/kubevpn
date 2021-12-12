package mesh

import (
	"context"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

type DeploymentController struct {
	factory   cmdutil.Factory
	clientset *kubernetes.Clientset
	namespace string
	name      string
	f         func() error
}

func NewDeploymentController(factory cmdutil.Factory, clientset *kubernetes.Clientset, namespace, name string) *DeploymentController {
	return &DeploymentController{
		factory:   factory,
		clientset: clientset,
		namespace: namespace,
		name:      name,
	}
}

func (c *DeploymentController) Inject() (map[string]string, *v1.PodSpec, error) {
	scale, err2 := c.clientset.AppsV1().Deployments(c.namespace).GetScale(context.TODO(), c.name, metav1.GetOptions{})
	if err2 != nil {
		return nil, nil, err2
	}
	c.f = func() error {
		_, err := c.clientset.AppsV1().Deployments(c.namespace).UpdateScale(
			context.TODO(),
			c.name,
			&autoscalingv1.Scale{
				ObjectMeta: metav1.ObjectMeta{Name: c.name, Namespace: c.namespace},
				Spec:       autoscalingv1.ScaleSpec{Replicas: scale.Spec.Replicas},
			},
			metav1.UpdateOptions{},
		)
		return err
	}
	_, err := c.clientset.AppsV1().Deployments(c.namespace).UpdateScale(
		context.TODO(),
		c.name,
		&autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{
				Name:      c.name,
				Namespace: c.namespace,
			},
			Spec: autoscalingv1.ScaleSpec{
				Replicas: int32(0),
			},
		},
		metav1.UpdateOptions{},
	)
	if err != nil {
		return nil, nil, err
	}
	get, err := c.clientset.AppsV1().Deployments(c.namespace).Get(context.TODO(), c.name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	return get.Spec.Template.Labels, &get.Spec.Template.Spec, nil
}

func (c *DeploymentController) Cancel() error {
	return c.f()
}
