package object

import (
	"errors"
	"fmt"

	api "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Pod is a stripped down api.Pod with only the items we need for CoreDNS.
type Pod struct {
	// Don't add new fields to this struct without talking to the CoreDNS maintainers.
	Version   string
	PodIP     string
	Name      string
	Namespace string
	Labels    map[string]string

	*Empty
}

var errPodTerminating = errors.New("pod terminating")

// ToPod converts an api.Pod to a *Pod.
func ToPod(obj meta.Object) (meta.Object, error) {
	apiPod, ok := obj.(*api.Pod)
	if !ok {
		return nil, fmt.Errorf("unexpected object %v", obj)
	}
	pod := &Pod{
		Version:   apiPod.GetResourceVersion(),
		PodIP:     apiPod.Status.PodIP,
		Namespace: apiPod.GetNamespace(),
		Name:      apiPod.GetName(),
		Labels:    apiPod.GetLabels(),
	}
	t := apiPod.ObjectMeta.DeletionTimestamp
	if t != nil && !(*t).Time.IsZero() {
		// if the pod is in the process of termination, return an error so it can be ignored
		// during add/update event processing
		return pod, errPodTerminating
	}

	*apiPod = api.Pod{}

	return pod, nil
}

var _ runtime.Object = &Pod{}

// DeepCopyObject implements the ObjectKind interface.
func (p *Pod) DeepCopyObject() runtime.Object {
	p1 := &Pod{
		Version:   p.Version,
		PodIP:     p.PodIP,
		Namespace: p.Namespace,
		Name:      p.Name,
	}
	return p1
}

// GetNamespace implements the metav1.Object interface.
func (p *Pod) GetNamespace() string { return p.Namespace }

// SetNamespace implements the metav1.Object interface.
func (p *Pod) SetNamespace(namespace string) {}

// GetName implements the metav1.Object interface.
func (p *Pod) GetName() string { return p.Name }

// SetName implements the metav1.Object interface.
func (p *Pod) SetName(name string) {}

// GetResourceVersion implements the metav1.Object interface.
func (p *Pod) GetResourceVersion() string { return p.Version }

// SetResourceVersion implements the metav1.Object interface.
func (p *Pod) SetResourceVersion(version string) {}
