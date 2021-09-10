package util

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	dockerterm "github.com/moby/term"
	log "github.com/sirupsen/logrus"
	"io"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	runtimeresource "k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	clientgowatch "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/cmd/exec"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

func WaitResource(clientset *kubernetes.Clientset, getter cache.Getter, namespace, apiVersion, kind string, list metav1.ListOptions, checker func(interface{}) bool) error {
	groupResources, _ := restmapper.GetAPIGroupResources(clientset)
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)
	groupVersionKind := schema.FromAPIVersionAndKind(apiVersion, kind)
	mapping, err := mapper.RESTMapping(groupVersionKind.GroupKind(), groupVersionKind.Version)
	if err != nil {
		log.Error(err)
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	watchlist := cache.NewFilteredListWatchFromClient(
		getter,
		mapping.Resource.Resource,
		namespace,
		func(options *metav1.ListOptions) {
			options.LabelSelector = list.LabelSelector
			options.FieldSelector = list.FieldSelector
			options.Watch = list.Watch
		},
	)

	preConditionFunc := func(store cache.Store) (bool, error) {
		if len(store.List()) == 0 {
			return false, nil
		}
		for _, p := range store.List() {
			if !checker(p) {
				return false, nil
			}
		}
		return true, nil
	}

	conditionFunc := func(e watch.Event) (bool, error) { return checker(e.Object), nil }

	object, err := scheme.Scheme.New(mapping.GroupVersionKind)
	if err != nil {
		return err
	}

	event, err := clientgowatch.UntilWithSync(ctx, watchlist, object, preConditionFunc, conditionFunc)
	if err != nil {
		log.Infof("wait to ready failed, error: %v, event: %v", err, event)
		return err
	}
	return nil
}

func GetAvailablePortOrDie() int {
	address, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:0", "0.0.0.0"))
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.ListenTCP("tcp", address)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func WaitPod(clientset *kubernetes.Clientset, namespace string, list metav1.ListOptions, checker func(*v1.Pod) bool) error {
	return WaitResource(
		clientset,
		clientset.CoreV1().RESTClient(),
		namespace,
		"v1",
		"Pod",
		list,
		func(i interface{}) bool { return checker(i.(*v1.Pod)) },
	)
}

func PortForwardPod(config *rest.Config, clientset *kubernetes.Clientset, podName, namespace, portPair string, readyChan, stopChan chan struct{}) error {
	url := clientset.CoreV1().
		RESTClient().
		Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward").
		URL()
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		log.Error(err)
		return err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url)
	p := []string{portPair}
	forwarder, err := portforward.New(dialer, p, stopChan, readyChan, os.Stdout, os.Stderr)
	if err != nil {
		log.Error(err)
		return err
	}

	if err = forwarder.ForwardPorts(); err != nil {
		log.Error(err)
		return err
	}
	return nil
}

func GetTopController(clientset *kubernetes.Clientset, namespace, serviceName string) (controller ResourceTuple) {
	labels, _ := GetLabels(clientset, namespace, serviceName)

	asSelector, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: labels})
	podList, _ := clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: asSelector.String(),
	})
	if len(podList.Items) == 0 {
		return
	}
	of := metav1.GetControllerOf(&podList.Items[0])
	for of != nil {
		b, err := clientset.AppsV1().RESTClient().Get().Namespace(namespace).
			Name(of.Name).Resource(strings.ToLower(of.Kind) + "s").Do(context.Background()).Raw()
		if k8serrors.IsNotFound(err) {
			return
		}
		var replicaSet appsv1.ReplicaSet
		if err = json.Unmarshal(b, &replicaSet); err == nil && len(replicaSet.Name) != 0 {
			controller.Resource = strings.ToLower(replicaSet.Kind) + "s"
			controller.Name = replicaSet.Name
			controller.Scale = *replicaSet.Spec.Replicas
			of = metav1.GetControllerOfNoCopy(&replicaSet)
			continue
		}
		var statefulSet appsv1.StatefulSet
		if err = json.Unmarshal(b, &statefulSet); err == nil && len(statefulSet.Name) != 0 {
			controller.Resource = strings.ToLower(statefulSet.Kind) + "s"
			controller.Name = statefulSet.Name
			controller.Scale = *statefulSet.Spec.Replicas
			of = metav1.GetControllerOfNoCopy(&statefulSet)
			continue
		}
		var deployment appsv1.Deployment
		if err = json.Unmarshal(b, &deployment); err == nil && len(deployment.Name) != 0 {
			controller.Resource = strings.ToLower(deployment.Kind) + "s"
			controller.Name = deployment.Name
			controller.Scale = *deployment.Spec.Replicas
			of = metav1.GetControllerOfNoCopy(&deployment)
			continue
		}
	}
	return
}

func UpdateReplicasScale(clientset *kubernetes.Clientset, namespace string, controller ResourceTuple) {
	err := retry.OnError(
		retry.DefaultRetry,
		func(err error) bool { return err != nil },
		func() error {
			result := &autoscalingv1.Scale{}
			err := clientset.AppsV1().RESTClient().Put().
				Namespace(namespace).
				Resource(controller.Resource).
				Name(controller.Name).
				SubResource("scale").
				VersionedParams(&metav1.UpdateOptions{}, scheme.ParameterCodec).
				Body(&autoscalingv1.Scale{
					ObjectMeta: metav1.ObjectMeta{
						Name:      controller.Name,
						Namespace: namespace,
					},
					Spec: autoscalingv1.ScaleSpec{
						Replicas: controller.Scale,
					},
				}).
				Do(context.Background()).
				Into(result)
			return err
		})
	if err != nil {
		log.Errorf("update scale: %s-%s's replicas to %d failed, error: %v", controller.Resource, controller.Name, controller.Scale, err)
	}
}

func Shell(clientset *kubernetes.Clientset, restclient *rest.RESTClient, config *rest.Config, podName, namespace, cmd string) (string, error) {
	pod, err := clientset.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})

	if err != nil {
		return "", err
	}
	if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
		err = fmt.Errorf("cannot exec into a container in a completed pod; current phase is %s", pod.Status.Phase)
		return "", err
	}
	containerName := pod.Spec.Containers[0].Name
	stdin, stdout, stderr := dockerterm.StdStreams()

	stdoutBuf := bytes.NewBuffer(nil)
	stdout = io.MultiWriter(stdout, stdoutBuf)
	StreamOptions := exec.StreamOptions{
		Namespace:     namespace,
		PodName:       podName,
		ContainerName: containerName,
		IOStreams:     genericclioptions.IOStreams{In: stdin, Out: stdout, ErrOut: stderr},
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
		req.VersionedParams(&v1.PodExecOptions{
			Container: containerName,
			Command:   []string{"sh", "-c", cmd},
			Stdin:     StreamOptions.Stdin,
			Stdout:    StreamOptions.Out != nil,
			Stderr:    StreamOptions.ErrOut != nil,
			TTY:       tt.Raw,
		}, scheme.ParameterCodec)
		return Executor.Execute("POST", req.URL(), config, StreamOptions.In, StreamOptions.Out, StreamOptions.ErrOut, tt.Raw, sizeQueue)
	}

	err = tt.Safe(fn)
	return strings.TrimRight(stdoutBuf.String(), "\n"), err
}

func IsWindows() bool {
	return runtime.GOOS == "windows"
}
func GetLabels(clientset *kubernetes.Clientset, namespace, serviceName string) (map[string]string, []v1.ContainerPort) {
	service, err := clientset.CoreV1().Services(namespace).Get(context.TODO(), serviceName, metav1.GetOptions{})
	if err != nil {
		log.Error(err)
		return nil, nil
	}
	selector := service.Spec.Selector
	newName := serviceName + "-" + "shadow"
	DeletePod(clientset, namespace, newName)
	var ports []v1.ContainerPort
	for _, port := range service.Spec.Ports {
		val := port.TargetPort.IntVal
		if val == 0 {
			val = port.Port
		}
		ports = append(ports, v1.ContainerPort{
			Name:          port.Name,
			ContainerPort: val,
			Protocol:      port.Protocol,
		})
	}
	return selector, ports
}

func DeletePod(clientset *kubernetes.Clientset, namespace, podName string) {
	zero := int64(0)
	err := clientset.CoreV1().Pods(namespace).Delete(context.TODO(), podName, metav1.DeleteOptions{
		GracePeriodSeconds: &zero,
	})
	if err != nil && k8serrors.IsNotFound(err) {
		log.Infof("not found shadow pod: %s, no need to delete it", podName)
	}
}

// TopLevelControllerSet record every pod's top level controller, like pod controllerBy replicaset, replicaset controllerBy deployment
var TopLevelControllerSet []ResourceTuple

type ResourceTuple struct {
	Resource string
	Name     string
	Scale    int32
}

// splitResourceTypeName handles type/name resource formats and returns a resource tuple
// (empty or not), whether it successfully found one, and an error
func SplitResourceTypeName(s string) (ResourceTuple, bool, error) {
	if !strings.Contains(s, "/") {
		return ResourceTuple{}, false, nil
	}
	seg := strings.Split(s, "/")
	if len(seg) != 2 {
		return ResourceTuple{}, false, fmt.Errorf("arguments in resource/name form may not have more than one slash")
	}
	resource, name := seg[0], seg[1]
	if len(resource) == 0 || len(name) == 0 || len(runtimeresource.SplitResourceArgument(resource)) != 1 {
		return ResourceTuple{}, false, fmt.Errorf("arguments in resource/name form must have a single resource and name")
	}
	return ResourceTuple{Resource: resource, Name: name}, true, nil
}
