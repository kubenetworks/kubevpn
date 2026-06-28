package util

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/moby/term"
	"errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/cmd/exec"
	"k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/podutils"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// PrintStatus writes a tabular summary of pod container statuses or conditions to the writer.
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

// GetEnv executes "env" in each container of the pod and returns a map of container names to temp file paths containing the environment output.
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

// WaitPod watches pods matching the list options until the checker function returns true or the context is cancelled.
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

// Shell executes a command in a pod container and returns the stdout output.
func Shell(ctx context.Context, clientset kubernetes.Interface, config *rest.Config, podName, containerName, ns string, cmd []string) (string, error) {
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

// AllContainersRunning reports whether the pod is ready and all containers have a ready status.
func AllContainersRunning(pod *corev1.Pod) bool {
	if !podutils.IsPodReady(pod) {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if !status.Ready {
			return false
		}
	}
	return true
}

// FindContainerEnv searches for an environment variable by key in the container spec and returns its value.
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

// FindContainerByName returns the container with the given name and its index, or (nil, -1) if not found.
func FindContainerByName(pod *corev1.Pod, name string) (*corev1.Container, int) {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i], i
		}
	}
	return nil, -1
}

// CheckPodStatus continuously watches the named pod and invokes cancelFunc if the pod is deleted or unreachable.
func CheckPodStatus(ctx context.Context, cancelFunc context.CancelFunc, podName string, podInterface v12.PodInterface) {
	var verifyAPIServerConnection = func() {
		err := retry.OnError(
			retry.DefaultBackoff,
			func(err error) bool {
				return err != nil
			},
			func() error {
				const podGetTimeout = 10 * time.Second
				ctx1, cancelFunc1 := context.WithTimeout(ctx, podGetTimeout)
				defer cancelFunc1()
				_, err := podInterface.Get(ctx1, podName, v1.GetOptions{})
				return err
			})
		if err != nil {
			plog.G(ctx).Errorf("Failed to get Pod %s: %v", podName, err)
			cancelFunc()
		}
	}

	const podStatusPollInterval = 5 * time.Second
	for ctx.Err() == nil {
		func() {
			defer time.Sleep(podStatusPollInterval)

			w, err := podInterface.Watch(ctx, v1.ListOptions{
				FieldSelector: fields.OneTermEqualSelector("metadata.name", podName).String(),
			})
			if err != nil {
				plog.G(ctx).Errorf("Failed to watch Pod %s: %v", podName, err)
				return
			}
			defer w.Stop()

			verifyAPIServerConnection()
			e, ok := <-w.ResultChan()
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
		}()
	}
}

// GetRunningPodList returns pods matching the label selector that are in Running phase with all containers ready.
func GetRunningPodList(ctx context.Context, clientset kubernetes.Interface, ns string, labelSelector string) ([]corev1.Pod, error) {
	list, err := clientset.CoreV1().Pods(ns).List(ctx, v1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(list.Items); i++ {
		if list.Items[i].GetDeletionTimestamp() != nil || !AllContainersRunning(&list.Items[i]) {
			list.Items = append(list.Items[:i], list.Items[i+1:]...)
			i--
		}
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("no running pod with label: %s", labelSelector)
	}
	return list.Items, nil
}

// DetectPodSupportIPv6 checks whether the traffic manager pod in the cluster has IPv6 enabled.
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

// GetPodIP returns all valid IP addresses assigned to the pod from its status.
func GetPodIP(pod corev1.Pod) []string {
	result := sets.New[string]()
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
