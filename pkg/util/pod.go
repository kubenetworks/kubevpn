package util

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/moby/term"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/cmd/exec"
	"k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/podutils"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
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

	if len(pod.Status.ContainerStatuses) == 0 && len(pod.Status.Conditions) != 0 {
		show("Type", "Reason", "Message")
		for _, condition := range pod.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
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

func GetEnv(ctx context.Context, set *kubernetes.Clientset, config *rest.Config, ns, podName string) (map[string]string, error) {
	pod, err := set.CoreV1().Pods(ns).Get(ctx, podName, v1.GetOptions{})
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for _, c := range pod.Spec.Containers {
		env, err := Shell(ctx, set, config, podName, c.Name, ns, []string{"env"})
		if err != nil {
			return nil, err
		}
		temp, err := os.CreateTemp("", "*.env")
		if err != nil {
			return nil, err
		}
		_, err = temp.WriteString(env)
		if err != nil {
			return nil, err
		}
		_ = temp.Close()
		result[Join(ns, c.Name)] = temp.Name()
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

func PortForwardPod(config *rest.Config, clientset *rest.RESTClient, podName, namespace string, portPair []string, readyChan chan struct{}, stopChan <-chan struct{}, out, errOut io.Writer) error {
	url := clientset.
		Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward").
		URL()
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		plog.G(context.Background()).Errorf("Create spdy roundtripper error: %s", err.Error())
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
	forwarder, err := portforward.New(dialer, portPair, stopChan, readyChan, out, errOut)
	if err != nil {
		return err
	}

	defer forwarder.Close()

	var errChan = make(chan error, 1)
	go func() {
		errChan <- forwarder.ForwardPorts()
	}()

	select {
	case err = <-errChan:
		return err
	case <-stopChan:
		return nil
	}
}

func Shell(ctx context.Context, clientset *kubernetes.Clientset, config *rest.Config, podName, containerName, ns string, cmd []string) (string, error) {
	stdin, _, _ := term.StdStreams()
	buf := bytes.NewBuffer(nil)
	errBuf := bytes.NewBuffer(nil)
	options := exec.ExecOptions{
		StreamOptions: exec.StreamOptions{
			Namespace:     ns,
			PodName:       podName,
			ContainerName: containerName,
			Stdin:         false,
			TTY:           false,
			Quiet:         true,
			IOStreams:     genericiooptions.IOStreams{In: stdin, Out: buf, ErrOut: errBuf},
		},
		Command:   cmd,
		Executor:  &DefaultRemoteExecutor{Ctx: ctx},
		PodClient: clientset.CoreV1(),
		Config:    config,
	}
	if err := options.Run(); err != nil {
		return errBuf.String(), err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
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

func CheckPodStatus(ctx context.Context, cancelFunc context.CancelFunc, podName string, podInterface v12.PodInterface) {
	var verifyAPIServerConnection = func() {
		err := retry.OnError(
			retry.DefaultBackoff,
			func(err error) bool {
				return err != nil
			},
			func() error {
				ctx1, cancelFunc1 := context.WithTimeout(ctx, time.Second*10)
				defer cancelFunc1()
				_, err := podInterface.Get(ctx1, podName, v1.GetOptions{})
				return err
			})
		if err != nil {
			plog.G(ctx).Errorf("Failed to get Pod %s: %v", podName, err)
			cancelFunc()
		}
	}

	for ctx.Err() == nil {
		func() {
			defer time.Sleep(time.Second * 5)

			w, err := podInterface.Watch(ctx, v1.ListOptions{
				FieldSelector: fields.OneTermEqualSelector("metadata.name", podName).String(),
			})
			if err != nil {
				plog.G(ctx).Errorf("Failed to watch Pod %s: %v", podName, err)
				return
			}
			defer w.Stop()

			verifyAPIServerConnection()
			select {
			case e, ok := <-w.ResultChan():
				if !ok {
					verifyAPIServerConnection()
					return
				}
				switch e.Type {
				case watch.Deleted:
					plog.G(ctx).Errorf("Pod %s is deleted", podName)
					cancelFunc()
					return
				case watch.Error:
					verifyAPIServerConnection()
					return
				case watch.Added, watch.Modified, watch.Bookmark:
					// do nothing
				}
			}
		}()
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
		return nil, fmt.Errorf("no running pod with label: %s", labelSelector)
	}
	return list.Items, nil
}

func DetectPodSupportIPv6(ctx context.Context, factory util.Factory, namespace string) (bool, error) {
	clientSet, err := factory.KubernetesClientSet()
	if err != nil {
		return false, err
	}
	restConfig, err := factory.ToRESTConfig()
	if err != nil {
		return false, err
	}
	label := fields.OneTermEqualSelector("app", config.ConfigMapPodTrafficManager).String()
	list, err := GetRunningPodList(ctx, clientSet, namespace, label)
	if err != nil {
		return false, err
	}
	cmd := []string{"cat", "/proc/sys/net/ipv6/conf/all/disable_ipv6"}
	shell, err := Shell(ctx, clientSet, restConfig, list[0].Name, config.ContainerSidecarVPN, namespace, cmd)
	if err != nil {
		return false, err
	}
	disableIPv6, err := strconv.Atoi(shell)
	if err != nil {
		return false, err
	}
	return disableIPv6 == 0, nil
}

func GetPodIP(pod corev1.Pod) []string {
	var result = sets.New[string]().Insert()
	for _, p := range pod.Status.PodIPs {
		if net.ParseIP(p.IP) != nil {
			result.Insert(p.IP)
		}
	}
	if net.ParseIP(pod.Status.PodIP) != nil {
		result.Insert(pod.Status.PodIP)
	}
	return result.UnsortedList()
}
