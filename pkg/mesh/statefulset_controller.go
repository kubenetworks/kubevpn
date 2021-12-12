package mesh

import (
	"context"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

type StatefulsetController struct {
	factory   cmdutil.Factory
	clientset *kubernetes.Clientset
	namespace string
	name      string
	f         func() error
}

func NewStatefulsetController(factory cmdutil.Factory, clientset *kubernetes.Clientset, namespace, name string) *StatefulsetController {
	return &StatefulsetController{
		factory:   factory,
		clientset: clientset,
		namespace: namespace,
		name:      name,
	}
}

func (s *StatefulsetController) Inject() (map[string]string, *v1.PodSpec, error) {
	statefulSet, err := s.clientset.AppsV1().StatefulSets(s.namespace).Get(context.TODO(), s.name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	s.f = func() error {
		_, err = s.clientset.AppsV1().
			StatefulSets(s.namespace).
			UpdateScale(context.TODO(), s.name, &autoscalingv1.Scale{
				ObjectMeta: metav1.ObjectMeta{
					Name:      s.name,
					Namespace: s.namespace,
				},
				Spec: autoscalingv1.ScaleSpec{
					Replicas: *statefulSet.Spec.Replicas,
				},
			}, metav1.UpdateOptions{})
		return err
	}
	_, err = s.clientset.AppsV1().
		StatefulSets(s.namespace).
		UpdateScale(context.TODO(), s.name, &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{
				Name:      s.name,
				Namespace: s.namespace,
			},
			Spec: autoscalingv1.ScaleSpec{
				Replicas: 0,
			},
		}, metav1.UpdateOptions{})
	if err != nil {
		return nil, nil, err
	}
	return statefulSet.Spec.Template.Labels, &statefulSet.Spec.Template.Spec, nil
}

func (s *StatefulsetController) Cancel() error {
	return s.f()
}
