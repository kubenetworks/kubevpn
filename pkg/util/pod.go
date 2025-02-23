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

	"github.com/distribution/reference"
	"github.com/hashicorp/go-version"
	"github.com/moby/term"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
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
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/cmd/exec"
	"k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	scheme2 "k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/podutils"
	pkgclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
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
		log.Errorf("Create spdy roundtripper error: %s", err.Error())
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
		log.Errorf("Create port forward error: %s", err.Error())
		return err
	}

	defer forwarder.Close()

	var errChan = make(chan error, 1)
	go func() {
		errChan <- forwarder.ForwardPorts()
	}()

	select {
	case err = <-errChan:
		log.Debugf("Forward port error: %v", err)
		return err
	case <-stopChan:
		return nil
	}
}

func GetTopOwnerReference(factory util.Factory, ns, workload string) (*resource.Info, error) {
	for {
		object, err := GetUnstructuredObject(factory, ns, workload)
		if err != nil {
			return nil, err
		}
		ownerReference := v1.GetControllerOf(object.Object.(*unstructured.Unstructured))
		if ownerReference == nil {
			return object, nil
		}
		workload = fmt.Sprintf("%s/%s", ownerReference.Kind, ownerReference.Name)
	}
}

// GetTopOwnerReferenceBySelector assume pods, controller has same labels
func GetTopOwnerReferenceBySelector(factory util.Factory, ns, selector string) (sets.Set[string], error) {
	object, err := GetUnstructuredObjectBySelector(factory, ns, selector)
	if err != nil {
		return nil, err
	}
	set := sets.New[string]()
	for _, info := range object {
		ownerReference, err := GetTopOwnerReference(factory, ns, fmt.Sprintf("%s/%s", info.Mapping.Resource.GroupResource().String(), info.Name))
		if err == nil && ownerReference.Mapping.Resource.Resource != "services" {
			set.Insert(fmt.Sprintf("%s/%s", ownerReference.Mapping.Resource.GroupResource().String(), ownerReference.Name))
		}
	}
	return set, nil
}

func Shell(_ context.Context, clientset *kubernetes.Clientset, config *rest.Config, podName, containerName, ns string, cmd []string) (string, error) {
	stdin, _, _ := term.StdStreams()
	buf := bytes.NewBuffer(nil)
	options := exec.ExecOptions{
		StreamOptions: exec.StreamOptions{
			Namespace:     ns,
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
	if err := options.Run(); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
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
				sb.WriteString(fmt.Sprintf("Pod %s is %s...\n", podT.Name, podT.Status.Phase))
				PrintStatus(podT, sb)

				if last != sb.String() {
					log.Infof(sb.String())
				}
				last = sb.String()
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
			log.Debugf("Failed to get Pod %s: %v", podName, err)
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
				log.Debugf("Failed to watch Pod %s: %v", podName, err)
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
					log.Debugf("Pod %s is deleted", podName)
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

func CheckPortStatus(ctx context.Context, cancelFunc context.CancelFunc, readyChan chan struct{}, localGvisorTCPPort string) {
	defer cancelFunc()
	ticker := time.NewTicker(time.Second * 60)
	defer ticker.Stop()

	select {
	case <-readyChan:
	case <-ticker.C:
		log.Debugf("Wait port-forward to be ready timeout")
		return
	case <-ctx.Done():
		return
	}

	for ctx.Err() == nil {
		var lc net.ListenConfig
		conn, err := lc.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", localGvisorTCPPort))
		if err == nil {
			_ = conn.Close()
			log.Debugf("Local port: %s is free", localGvisorTCPPort)
			return
		}
		time.Sleep(time.Second * 1)
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

// UpdateImage update to newer image
func UpdateImage(ctx context.Context, factory util.Factory, ns string, deployName string, image string) error {
	clientSet, err2 := factory.KubernetesClientSet()
	if err2 != nil {
		return err2
	}
	deployment, err := clientSet.AppsV1().Deployments(ns).Get(ctx, deployName, v1.GetOptions{})
	if err != nil {
		return err
	}
	origin := deployment.DeepCopy()
	newImg, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return err
	}
	newTag, ok := newImg.(reference.NamedTagged)
	if !ok {
		return nil
	}
	oldImg, err := reference.ParseNormalizedNamed(deployment.Spec.Template.Spec.Containers[0].Image)
	if err != nil {
		return err
	}
	var oldTag reference.NamedTagged
	oldTag, ok = oldImg.(reference.NamedTagged)
	if !ok {
		return nil
	}
	if reference.Domain(newImg) != reference.Domain(oldImg) {
		return nil
	}
	var oldVersion, newVersion *version.Version
	oldVersion, err = version.NewVersion(oldTag.Tag())
	if err != nil {
		return nil
	}
	newVersion, err = version.NewVersion(newTag.Tag())
	if err != nil {
		return nil
	}
	if oldVersion.GreaterThanOrEqual(newVersion) {
		return nil
	}

	log.Infof("Found newer image %s, set image from %s to it...", image, deployment.Spec.Template.Spec.Containers[0].Image)
	for i := range deployment.Spec.Template.Spec.Containers {
		deployment.Spec.Template.Spec.Containers[i].Image = image
	}
	p := pkgclient.MergeFrom(deployment)
	data, err := pkgclient.MergeFrom(origin).Data(deployment)
	if err != nil {
		return err
	}
	_, err = clientSet.AppsV1().Deployments(ns).Patch(ctx, deployName, p.Type(), data, v1.PatchOptions{})
	if err != nil {
		return err
	}
	err = RolloutStatus(ctx, factory, ns, fmt.Sprintf("deployments/%s", deployName), time.Minute*60)
	return err
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
