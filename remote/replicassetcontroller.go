package remote

import (
	"context"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

type ReplicasController struct {
	factory   cmdutil.Factory
	clientset *kubernetes.Clientset
	namespace string
	name      string
	f         func() error
}

func NewReplicasController(factory cmdutil.Factory, clientset *kubernetes.Clientset, namespace, name string) *ReplicasController {
	return &ReplicasController{
		factory:   factory,
		clientset: clientset,
		namespace: namespace,
		name:      name,
	}
}

func (c *ReplicasController) Inject() (map[string]string, *v1.PodSpec, error) {
	replicaSet, err2 := c.clientset.AppsV1().ReplicaSets(c.namespace).Get(context.TODO(), c.name, metav1.GetOptions{})
	if err2 != nil {
		return nil, nil, err2
	}
	_, err := c.clientset.AppsV1().ReplicaSets(c.namespace).UpdateScale(context.TODO(), c.name, &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.name,
			Namespace: c.namespace,
		},
		Spec: autoscalingv1.ScaleSpec{
			Replicas: int32(0),
		},
	}, metav1.UpdateOptions{})
	if err != nil {
		return nil, nil, err
	}
	c.f = func() error {
		_, err = c.clientset.AppsV1().ReplicaSets(c.namespace).
			UpdateScale(context.TODO(), c.name, &autoscalingv1.Scale{
				ObjectMeta: metav1.ObjectMeta{
					Name:      c.name,
					Namespace: c.namespace,
				},
				Spec: autoscalingv1.ScaleSpec{
					Replicas: *replicaSet.Spec.Replicas,
				},
			}, metav1.UpdateOptions{})
		return err
	}
	return replicaSet.Spec.Template.Labels, &replicaSet.Spec.Template.Spec, nil
}

func (c *ReplicasController) Cancel() error {
	return c.f()
}
