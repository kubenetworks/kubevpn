package util

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/moby/term"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/exp/constraints"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/kubectl/pkg/cmd/exec"
	"k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	scheme2 "k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/podutils"
)

type PodRouteConfig struct {
	LocalTunIPv4 string
	LocalTunIPv6 string
}

func PrintStatus(pod *corev1.Pod, writer io.Writer) {
	w := tabwriter.NewWriter(writer, 1, 1, 1, ' ', 0)
	defer w.Flush()
	show := func(name string, v1, v2 any) {
		_, _ = fmt.Fprintf(w, "%s\t%v\t%v\n", name, v1, v2)
	}

	if len(pod.Status.ContainerStatuses) == 0 {
		show("Type", "Reason", "Message")
		for _, condition := range pod.Status.Conditions {
			if condition.Status == corev1.ConditionFalse {
				show(string(condition.Type), condition.Reason, condition.Message)
			}
		}
		return
	}
	show("Container", "Reason", "Message")
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Waiting != nil {
			show(status.Name, status.State.Waiting.Reason, status.State.Waiting.Message)
		}
		if status.State.Running != nil {
			show(status.Name, "ContainerRunning", "")
		}
		if status.State.Terminated != nil {
			show(status.Name, status.State.Terminated.Reason, status.State.Terminated.Message)
		}
	}
}

func PrintStatusInline(pod *corev1.Pod) string {
	var sb = bytes.NewBuffer(nil)
	w := tabwriter.NewWriter(sb, 1, 1, 1, ' ', 0)
	show := func(v1, v2 any) {
		_, _ = fmt.Fprintf(w, "%v\t\t%v", v1, v2)
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Waiting != nil {
			show(status.State.Waiting.Reason, status.State.Waiting.Message)
		}
		if status.State.Running != nil {
			show("ContainerRunning", "")
		}
		if status.State.Terminated != nil {
			show(status.State.Terminated.Reason, status.State.Terminated.Message)
		}
	}
	_ = w.Flush()
	return sb.String()
}

func max[T constraints.Ordered](a T, b T) T {
	if a > b {
		return a
	}
	return b
}

func GetEnv(ctx context.Context, f util.Factory, ns, pod string) (map[string][]string, error) {
	set, err2 := f.KubernetesClientSet()
	if err2 != nil {
		return nil, err2
	}
	config, err2 := f.ToRESTConfig()
	if err2 != nil {
		return nil, err2
	}
	get, err := set.CoreV1().Pods(ns).Get(ctx, pod, v1.GetOptions{})
	if err != nil {
		return nil, err
	}
	result := map[string][]string{}
	for _, c := range get.Spec.Containers {
		env, err := Shell(ctx, set, config, pod, c.Name, ns, []string{"env"})
		if err != nil {
			return nil, err
		}
		split := strings.Split(env, "\n")
		result[c.Name] = split
	}
	return result, nil
}

func WaitPod(ctx context.Context, podInterface v12.PodInterface, list v1.ListOptions, checker func(*corev1.Pod) bool) error {
	w, err := podInterface.Watch(ctx, list)
	if err != nil {
		return err
	}
	defer w.Stop()
	for {
		select {
		case e := <-w.ResultChan():
			if pod, ok := e.Object.(*corev1.Pod); ok {
				if checker(pod) {
					return nil
				}
			}
		case <-ctx.Done():
			return errors.New("wait for pod to be ready timeout")
		}
	}
}

func PortForwardPod(config *rest.Config, clientset *rest.RESTClient, podName, namespace string, portPair []string, readyChan chan struct{}, stopChan <-chan struct{}) error {
	err := os.Setenv(string(util.RemoteCommandWebsockets), "true")
	if err != nil {
		return err
	}
	url := clientset.
		Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward").
		URL()
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		logrus.Errorf("create spdy roundtripper error: %s", err.Error())
		return err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url)
	// Legacy SPDY executor is default. If feature gate enabled, fallback
	// executor attempts websockets first--then SPDY.
	if util.RemoteCommandWebsockets.IsEnabled() {
		// WebSocketExecutor must be "GET" method as described in RFC 6455 Sec. 4.1 (page 17).
		websocketDialer, err := portforward.NewSPDYOverWebsocketDialer(url, config)
		if err != nil {
			return err
		}
		dialer = portforward.NewFallbackDialer(websocketDialer, dialer, httpstream.IsUpgradeFailure)
	}
	forwarder, err := portforward.New(dialer, portPair, stopChan, readyChan, nil, os.Stderr)
	if err != nil {
		logrus.Errorf("create port forward error: %s", err.Error())
		return err
	}

	if err = forwarder.ForwardPorts(); err != nil {
		logrus.Errorf("forward port error: %s", err.Error())
		return err
	}
	return nil
}

func GetTopOwnerReference(factory util.Factory, namespace, workload string) (*resource.Info, error) {
	for {
		object, err := GetUnstructuredObject(factory, namespace, workload)
		if err != nil {
			return nil, err
		}
		ownerReference := v1.GetControllerOf(object.Object.(*unstructured.Unstructured))
		if ownerReference == nil {
			return object, nil
		}
		// apiVersion format is Group/Version is like: apps/v1, apps.kruise.io/v1beta1
		version, err := schema.ParseGroupVersion(ownerReference.APIVersion)
		if err != nil {
			return object, nil
		}
		gk := v1.GroupKind{
			Group: version.Group,
			Kind:  ownerReference.Kind,
		}
		workload = fmt.Sprintf("%s/%s", gk.String(), ownerReference.Name)
	}
}

// GetTopOwnerReferenceBySelector assume pods, controller has same labels
func GetTopOwnerReferenceBySelector(factory util.Factory, namespace, selector string) (sets.Set[string], error) {
	object, err := GetUnstructuredObjectBySelector(factory, namespace, selector)
	if err != nil {
		return nil, err
	}
	set := sets.New[string]()
	for _, info := range object {
		reference, err := GetTopOwnerReference(factory, namespace, fmt.Sprintf("%s/%s", info.Mapping.Resource.GroupResource().String(), info.Name))
		if err == nil && reference.Mapping.Resource.Resource != "services" {
			set.Insert(fmt.Sprintf("%s/%s", reference.Mapping.GroupVersionKind.GroupKind().String(), reference.Name))
		}
	}
	return set, nil
}

func Shell(_ context.Context, clientset *kubernetes.Clientset, config *rest.Config, podName, containerName, namespace string, cmd []string) (string, error) {
	err := os.Setenv(string(util.RemoteCommandWebsockets), "true")
	if err != nil {
		return "", err
	}
	stdin, _, _ := term.StdStreams()
	buf := bytes.NewBuffer(nil)
	options := exec.ExecOptions{
		StreamOptions: exec.StreamOptions{
			Namespace:     namespace,
			PodName:       podName,
			ContainerName: containerName,
			Stdin:         false,
			TTY:           false,
			Quiet:         true,
			IOStreams:     genericiooptions.IOStreams{In: stdin, Out: buf, ErrOut: io.Discard},
		},
		Command:   cmd,
		Executor:  &exec.DefaultRemoteExecutor{},
		PodClient: clientset.CoreV1(),
		Config:    config,
	}
	if err = options.Run(); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), err
}

func WaitPodToBeReady(ctx context.Context, podInterface v12.PodInterface, selector v1.LabelSelector) error {
	watchStream, err := podInterface.Watch(ctx, v1.ListOptions{
		LabelSelector: fields.SelectorFromSet(selector.MatchLabels).String(),
	})
	if err != nil {
		return err
	}
	defer watchStream.Stop()
	var last string
	ticker := time.NewTicker(time.Minute * 60)
	defer ticker.Stop()
	for {
		select {
		case e, ok := <-watchStream.ResultChan():
			if !ok {
				return fmt.Errorf("can not wait pod to be ready because of watch chan has closed")
			}
			if podT, ok := e.Object.(*corev1.Pod); ok {
				if podT.DeletionTimestamp != nil {
					continue
				}
				var sb = bytes.NewBuffer(nil)
				sb.WriteString(fmt.Sprintf("pod [%s] status is %s\n", podT.Name, podT.Status.Phase))
				PrintStatus(podT, sb)

				if last != sb.String() {
					logrus.Infof(sb.String())
				}
				if podutils.IsPodReady(podT) && func() bool {
					for _, status := range podT.Status.ContainerStatuses {
						if !status.Ready {
							return false
						}
					}
					return true
				}() {
					return nil
				}
				last = sb.String()
			}
		case <-ticker.C:
			return errors.New(fmt.Sprintf("wait pod to be ready timeout"))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func AllContainerIsRunning(pod *corev1.Pod) bool {
	isReady := podutils.IsPodReady(pod)
	if !isReady {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if !status.Ready {
			return false
		}
	}
	return true
}

func FindContainerEnv(container *corev1.Container, key string) (value string, found bool) {
	if container == nil {
		return
	}
	for _, envVar := range container.Env {
		if envVar.Name == key {
			value = envVar.Value
			found = true
			return
		}
	}
	return
}

func FindContainerByName(pod *corev1.Pod, name string) (*corev1.Container, int) {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i], i
		}
	}
	return nil, -1
}

func CheckPodStatus(cCtx context.Context, cFunc context.CancelFunc, podName string, podInterface v12.PodInterface) {
	w, err := podInterface.Watch(cCtx, v1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", podName).String(),
	})
	if err != nil {
		return
	}
	defer w.Stop()
	for {
		select {
		case e, ok := <-w.ResultChan():
			if !ok {
				return
			}
			switch e.Type {
			case watch.Deleted:
				cFunc()
				return
			case watch.Error:
				return
			case watch.Added, watch.Modified, watch.Bookmark:
				// do nothing
			default:
				return
			}
		case <-cCtx.Done():
			return
		}
	}
}

func Rollback(f util.Factory, ns, workload string) {
	r := f.NewBuilder().
		WithScheme(scheme2.Scheme, scheme2.Scheme.PrioritizedVersionsAllGroups()...).
		NamespaceParam(ns).DefaultNamespace().
		ResourceTypeOrNameArgs(true, workload).
		ContinueOnError().
		Latest().
		Flatten().
		Do()
	if r.Err() == nil {
		_ = r.Visit(func(info *resource.Info, err error) error {
			if err != nil {
				return err
			}
			rollbacker, err := polymorphichelpers.RollbackerFn(f, info.ResourceMapping())
			if err != nil {
				return err
			}
			_, err = rollbacker.Rollback(info.Object, nil, 0, util.DryRunNone)
			return err
		})
	}
}

func GetRunningPodList(ctx context.Context, clientset *kubernetes.Clientset, ns string, labelSelector string) ([]corev1.Pod, error) {
	list, err := clientset.CoreV1().Pods(ns).List(ctx, v1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(list.Items); i++ {
		if list.Items[i].GetDeletionTimestamp() != nil || !AllContainerIsRunning(&list.Items[i]) {
			list.Items = append(list.Items[:i], list.Items[i+1:]...)
			i--
		}
	}
	if len(list.Items) == 0 {
		return nil, errors.New("can not found any running pod")
	}
	return list.Items, nil
}
