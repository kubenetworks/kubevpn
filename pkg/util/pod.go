package util

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
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
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/kubectl/pkg/cmd/exec"
	"k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/podutils"

	"github.com/wencaiwulue/kubevpn/pkg/config"
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
	client, err2 := f.RESTClient()
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
		env, err := Shell(set, client, config, pod, c.Name, ns, []string{"env"})
		if err != nil {
			return nil, err
		}
		split := strings.Split(env, "\n")
		result[c.Name] = split
	}
	return result, nil
}

func Heartbeats() {
	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()

	for ; true; <-ticker.C {
		for _, ip := range []net.IP{config.RouterIP, config.RouterIP6} {
			time.Sleep(time.Millisecond * time.Duration(rand.Intn(1000)))
			_, _ = Ping(ip.String())
		}
	}
}

func WaitPod(podInterface v12.PodInterface, list v1.ListOptions, checker func(*corev1.Pod) bool) error {
	ctx, cancelFunc := context.WithTimeout(context.Background(), time.Second*30)
	defer cancelFunc()
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
	forwarder, err := portforward.NewOnAddresses(dialer, []string{"localhost"}, portPair, stopChan, readyChan, nil, os.Stderr)
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

func Shell(clientset *kubernetes.Clientset, restclient *rest.RESTClient, config *rest.Config, podName, containerName, namespace string, cmd []string) (string, error) {
	pod, err := clientset.CoreV1().Pods(namespace).Get(context.Background(), podName, v1.GetOptions{})

	if err != nil {
		return "", err
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		err = fmt.Errorf("cannot exec into a container in a completed pod; current phase is %s", pod.Status.Phase)
		return "", err
	}
	if containerName == "" {
		containerName = pod.Spec.Containers[0].Name
	}
	stdin, _, _ := term.StdStreams()

	stdoutBuf := bytes.NewBuffer(nil)
	stdout := io.MultiWriter(stdoutBuf)
	StreamOptions := exec.StreamOptions{
		Namespace:     namespace,
		PodName:       podName,
		ContainerName: containerName,
		IOStreams:     genericclioptions.IOStreams{In: stdin, Out: stdout, ErrOut: nil},
	}
	Executor := &exec.DefaultRemoteExecutor{}
	// ensure we can recover the terminal while attached
	tt := StreamOptions.SetupTTY()

	var sizeQueue remotecommand.TerminalSizeQueue
	if tt.Raw {
		// this call spawns a goroutine to monitor/update the terminal size
		sizeQueue = tt.MonitorSize(tt.GetSize())

		// unset p.Err if it was previously set because both stdout and stderr go over p.Out when tty is
		// true
		StreamOptions.ErrOut = nil
	}

	fn := func() error {
		req := restclient.Post().
			Resource("pods").
			Name(pod.Name).
			Namespace(pod.Namespace).
			SubResource("exec")
		req.VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   cmd,
			Stdin:     StreamOptions.Stdin,
			Stdout:    StreamOptions.Out != nil,
			Stderr:    StreamOptions.ErrOut != nil,
			TTY:       tt.Raw,
		}, scheme.ParameterCodec)
		return Executor.Execute(req.URL(), config, StreamOptions.In, StreamOptions.Out, StreamOptions.ErrOut, tt.Raw, sizeQueue)
	}

	err = tt.Safe(fn)
	return strings.TrimRight(stdoutBuf.String(), "\n"), err
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
		case <-time.Tick(time.Minute * 60):
			return errors.New(fmt.Sprintf("wait pod to be ready timeout"))
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
