package util

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	dockerterm "github.com/moby/term"
	"github.com/pkg/errors"
	probing "github.com/prometheus-community/pro-bing"
	log "github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	runtimeresource "k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/remotecommand"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/cmd/exec"
	"k8s.io/kubectl/pkg/cmd/util"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/podutils"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/driver"
)

func GetAvailableUDPPortOrDie() int {
	address, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:0", "0.0.0.0"))
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.ListenUDP("udp", address)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	return listener.LocalAddr().(*net.UDPAddr).Port
}

func GetAvailableTCPPortOrDie() int {
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

func WaitPod(podInterface v12.PodInterface, list metav1.ListOptions, checker func(*v1.Pod) bool) error {
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
			if pod, ok := e.Object.(*v1.Pod); ok {
				if checker(pod) {
					return nil
				}
			}
		case <-ctx.Done():
			return errors.New("wait for pod to be ready timeout")
		}
	}
}

func PortForwardPod(config *rest.Config, clientset *rest.RESTClient, podName, namespace, port string, readyChan chan struct{}, stopChan <-chan struct{}) error {
	url := clientset.
		Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward").Timeout(time.Second * 30).
		MaxRetries(3).
		URL()
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		log.Error(err)
		return err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport, Timeout: time.Second * 30}, "POST", url)
	p := []string{port}
	forwarder, err := NewOnAddresses(dialer, []string{"0.0.0.0"}, p, stopChan, readyChan, nil, os.Stderr)
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

func GetTopOwnerReference(factory cmdutil.Factory, namespace, workload string) (*runtimeresource.Info, error) {
	for {
		object, err := GetUnstructuredObject(factory, namespace, workload)
		if err != nil {
			return nil, err
		}
		ownerReference := metav1.GetControllerOf(object.Object.(*unstructured.Unstructured))
		if ownerReference == nil {
			return object, nil
		}
		// apiVersion format is Group/Version is like: apps/v1, apps.kruise.io/v1beta1
		version, err := schema.ParseGroupVersion(ownerReference.APIVersion)
		if err != nil {
			return object, nil
		}
		gk := metav1.GroupKind{
			Group: version.Group,
			Kind:  ownerReference.Kind,
		}
		workload = fmt.Sprintf("%s/%s", gk.String(), ownerReference.Name)
	}
}

// GetTopOwnerReferenceBySelector assume pods, controller has same labels
func GetTopOwnerReferenceBySelector(factory cmdutil.Factory, namespace, selector string) (sets.Set[string], error) {
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
	pod, err := clientset.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})

	if err != nil {
		return "", err
	}
	if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
		err = fmt.Errorf("cannot exec into a container in a completed pod; current phase is %s", pod.Status.Phase)
		return "", err
	}
	if containerName == "" {
		containerName = pod.Spec.Containers[0].Name
	}
	stdin, _, _ := dockerterm.StdStreams()

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
		req.VersionedParams(&v1.PodExecOptions{
			Container: containerName,
			Command:   cmd,
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

func GetUnstructuredObject(f cmdutil.Factory, namespace string, workloads string) (*runtimeresource.Info, error) {
	do := f.NewBuilder().
		Unstructured().
		NamespaceParam(namespace).DefaultNamespace().AllNamespaces(false).
		ResourceTypeOrNameArgs(true, workloads).
		ContinueOnError().
		Latest().
		Flatten().
		TransformRequests(func(req *rest.Request) { req.Param("includeObject", "Object") }).
		Do()
	if err := do.Err(); err != nil {
		log.Warn(err)
		return nil, err
	}
	infos, err := do.Infos()
	if err != nil {
		log.Println(err)
		return nil, err
	}
	if len(infos) == 0 {
		return nil, fmt.Errorf("not found workloads %s", workloads)
	}
	return infos[0], err
}

func GetUnstructuredObjectList(f cmdutil.Factory, namespace string, workloads []string) ([]*runtimeresource.Info, error) {
	do := f.NewBuilder().
		Unstructured().
		NamespaceParam(namespace).DefaultNamespace().AllNamespaces(false).
		ResourceTypeOrNameArgs(true, workloads...).
		ContinueOnError().
		Latest().
		Flatten().
		TransformRequests(func(req *rest.Request) { req.Param("includeObject", "Object") }).
		Do()
	if err := do.Err(); err != nil {
		log.Warn(err)
		return nil, err
	}
	infos, err := do.Infos()
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 {
		return nil, fmt.Errorf("not found resource %v", workloads)
	}
	return infos, err
}

func GetUnstructuredObjectBySelector(f cmdutil.Factory, namespace string, selector string) ([]*runtimeresource.Info, error) {
	do := f.NewBuilder().
		Unstructured().
		NamespaceParam(namespace).DefaultNamespace().AllNamespaces(false).
		ResourceTypeOrNameArgs(true, "all").
		LabelSelector(selector).
		ContinueOnError().
		Latest().
		Flatten().
		TransformRequests(func(req *rest.Request) { req.Param("includeObject", "Object") }).
		Do()
	if err := do.Err(); err != nil {
		log.Warn(err)
		return nil, err
	}
	infos, err := do.Infos()
	if err != nil {
		log.Println(err)
		return nil, err
	}
	if len(infos) == 0 {
		return nil, errors.New("Not found")
	}
	return infos, err
}

func GetPodTemplateSpecPath(u *unstructured.Unstructured) (*v1.PodTemplateSpec, []string, error) {
	var stringMap map[string]interface{}
	var b bool
	var err error
	var path []string
	if stringMap, b, err = unstructured.NestedMap(u.Object, "spec", "template"); b && err == nil {
		path = []string{"spec", "template"}
	} else if stringMap, b, err = unstructured.NestedMap(u.Object); b && err == nil {
		path = []string{}
	} else {
		return nil, nil, err
	}
	marshal, err := json.Marshal(stringMap)
	if err != nil {
		return nil, nil, err
	}
	var p v1.PodTemplateSpec
	if err = json.Unmarshal(marshal, &p); err != nil {
		return nil, nil, err
	}
	return &p, path, nil
}

func BytesToInt(b []byte) uint32 {
	buffer := bytes.NewBuffer(b)
	var u uint32
	if err := binary.Read(buffer, binary.BigEndian, &u); err != nil {
		log.Warn(err)
	}
	return u
}

func Ping(targetIP string) (bool, error) {
	pinger, err := probing.NewPinger(targetIP)
	if err != nil {
		return false, err
	}
	pinger.SetLogger(nil)
	pinger.SetPrivileged(true)
	pinger.Count = 3
	pinger.Timeout = time.Millisecond * 1500
	err = pinger.Run() // Blocks until finished.
	if err != nil {
		return false, err
	}
	stat := pinger.Statistics()
	return stat.PacketsRecv == stat.PacketsSent, err
}

func RolloutStatus(ctx1 context.Context, factory cmdutil.Factory, namespace, workloads string, timeout time.Duration) error {
	client, _ := factory.DynamicClient()
	r := factory.NewBuilder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		NamespaceParam(namespace).DefaultNamespace().
		ResourceTypeOrNameArgs(true, workloads).
		SingleResourceType().
		Latest().
		Do()
	err := r.Err()
	if err != nil {
		return err
	}

	infos, err := r.Infos()
	if err != nil {
		return err
	}
	if len(infos) != 1 {
		return fmt.Errorf("rollout status is only supported on individual resources and resource collections - %d resources were found", len(infos))
	}
	info := infos[0]
	mapping := info.ResourceMapping()

	statusViewer, err := polymorphichelpers.StatusViewerFn(mapping)
	if err != nil {
		return err
	}

	fieldSelector := fields.OneTermEqualSelector("metadata.name", info.Name).String()
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (k8sruntime.Object, error) {
			options.FieldSelector = fieldSelector
			return client.Resource(info.Mapping.Resource).Namespace(info.Namespace).List(context.Background(), options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			options.FieldSelector = fieldSelector
			return client.Resource(info.Mapping.Resource).Namespace(info.Namespace).Watch(context.Background(), options)
		},
	}

	// if the rollout isn't done yet, keep watching deployment status
	ctx, cancel := watchtools.ContextWithOptionalTimeout(ctx1, timeout)
	defer cancel()
	return func() error {
		_, err = watchtools.UntilWithSync(ctx, lw, &unstructured.Unstructured{}, nil, func(e watch.Event) (bool, error) {
			switch t := e.Type; t {
			case watch.Added, watch.Modified:
				status, done, err := statusViewer.Status(e.Object.(k8sruntime.Unstructured), 0)
				if err != nil {
					return false, err
				}
				_, _ = fmt.Fprintf(os.Stdout, "%s", status)
				// Quit waiting if the rollout is done
				if done {
					return true, nil
				}

				return false, nil

			case watch.Deleted:
				// We need to abort to avoid cases of recreation and not to silently watch the wrong (new) object
				return true, fmt.Errorf("object has been deleted")

			default:
				return true, fmt.Errorf("internal error: unexpected event %#v", e)
			}
		})
		return err
	}()
}

type proxyWriter struct {
	*bytes.Buffer
	checker func(log string)
}

func (w *proxyWriter) Write(b []byte) (int, error) {
	write, err := w.Buffer.Write(b)
	if w.checker != nil {
		w.checker(w.Buffer.String())
	}
	return write, err
}

func RunWithRollingOutWithChecker(cmd *osexec.Cmd, checker func(log string)) (string, string, error) {
	stdoutBuf := &proxyWriter{Buffer: bytes.NewBuffer(make([]byte, 0)), checker: checker}
	stderrBuf := &proxyWriter{Buffer: bytes.NewBuffer(make([]byte, 0)), checker: checker}

	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()
	stdout := io.MultiWriter(os.Stdout, stdoutBuf)
	stderr := io.MultiWriter(os.Stderr, stderrBuf)
	go func() {
		_, _ = io.Copy(stdout, stdoutPipe)
	}()
	go func() {
		_, _ = io.Copy(stderr, stderrPipe)
	}()
	if err := cmd.Start(); err != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return stdoutBuf.String(), stderrBuf.String(), err
	}
	_ = cmd.Wait()
	var err error
	if !cmd.ProcessState.Success() {
		err = errors.New("exit code is not 0")
	}

	stdoutStr := strings.TrimSpace(stdoutBuf.String())
	stderrStr := strings.TrimSpace(stderrBuf.String())

	return stdoutStr, stderrStr, err
}

func WaitPortToBeFree(ctx context.Context, port int) error {
	log.Infoln(fmt.Sprintf("wait port %v to be free...", port))
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait port %v to be free timeout", port)
		case <-time.Tick(time.Second * 2):
			if !IsPortListening(port) {
				log.Infoln(fmt.Sprintf("port %v are free", port))
				return nil
			}
		}
	}
}

func IsPortListening(port int) bool {
	listener, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return true
	} else {
		_ = listener.Close()
		return false
	}
}

func GetAnnotation(f util.Factory, ns string, resources string) (map[string]string, error) {
	ownerReference, err := GetTopOwnerReference(f, ns, resources)
	if err != nil {
		return nil, err
	}
	u, ok := ownerReference.Object.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("can not convert to unstaructed")
	}
	annotations := u.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	return annotations, nil
}

func CanI(clientset *kubernetes.Clientset, sa, ns string, resource *rbacv1.PolicyRule) (allowed bool, err error) {
	var roleBindingList *rbacv1.RoleBindingList
	roleBindingList, err = clientset.RbacV1().RoleBindings(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for _, item := range roleBindingList.Items {
		for _, subject := range item.Subjects {
			if subject.Name == sa && subject.Kind == "ServiceAccount" {
				var role *rbacv1.Role
				role, err = clientset.RbacV1().Roles(ns).Get(context.Background(), item.RoleRef.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				for _, rule := range role.Rules {
					if sets.New[string](rule.Resources...).HasAll(resource.Resources...) && sets.New[string](rule.Verbs...).HasAll(resource.Verbs...) {
						if len(rule.ResourceNames) == 0 || sets.New[string](rule.ResourceNames...).HasAll(resource.ResourceNames...) {
							return true, nil
						}
					}
				}
			}
		}
	}

	var clusterRoleBindingList *rbacv1.ClusterRoleBindingList
	clusterRoleBindingList, err = clientset.RbacV1().ClusterRoleBindings().List(context.Background(), metav1.ListOptions{})
	for _, item := range clusterRoleBindingList.Items {
		for _, subject := range item.Subjects {
			if subject.Name == sa && subject.Kind == "ServiceAccount" {
				var role *rbacv1.ClusterRole
				role, err = clientset.RbacV1().ClusterRoles().Get(context.Background(), item.RoleRef.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				for _, rule := range role.Rules {
					if sets.New[string](rule.Resources...).HasAll(resource.Resources...) && sets.New[string](rule.Verbs...).HasAll(resource.Verbs...) {
						if len(rule.ResourceNames) == 0 || sets.New[string](rule.ResourceNames...).HasAll(resource.ResourceNames...) {
							return true, nil
						}
					}
				}
			}
		}
	}

	return false, nil
}

func DoReq(request *http.Request) (body []byte, err error) {
	cert, ok := os.LookupEnv(config.TLSCertKey)
	if !ok {
		return nil, fmt.Errorf("can not get %s from env", config.TLSCertKey)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM([]byte(cert))

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: caCertPool,
			},
		},
	}

	var resp *http.Response
	resp, err = client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("err: %v", err)
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("can not read body, err: %v", err)
	}
	if resp.StatusCode == http.StatusOK {
		return body, nil
	}
	return body, fmt.Errorf("http status is %d", resp.StatusCode)
}

func GetTlsDomain(namespace string) string {
	return config.ConfigMapPodTrafficManager + "." + namespace + "." + "svc"
}

func IsIPv4(packet []byte) bool {
	return 4 == (packet[0] >> 4)
}

func IsIPv6(packet []byte) bool {
	return 6 == (packet[0] >> 4)
}

func Deduplicate(cidr []*net.IPNet) (result []*net.IPNet) {
	var set = sets.New[string]()
	for _, ipNet := range cidr {
		if !set.Has(ipNet.String()) {
			result = append(result, ipNet)
		}
		set.Insert(ipNet.String())
	}
	return
}

func AllContainerIsRunning(pod *v1.Pod) bool {
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

func CleanExtensionLib() {
	if !IsWindows() {
		return
	}
	path, err := os.Executable()
	if err != nil {
		return
	}
	filename := filepath.Join(filepath.Dir(path), "wintun.dll")
	_ = retry.OnError(
		retry.DefaultRetry,
		func(error) bool {
			_, err = os.Lstat(filename)
			return !errors.Is(err, os.ErrNotExist)
		},
		func() error {
			err = driver.UninstallWireGuardTunDriver()
			return fmt.Errorf("%v", err)
		},
	)
	_, err = os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	MoveToTemp()
}

func WaitPodToBeReady(ctx context.Context, podInterface v12.PodInterface, selector metav1.LabelSelector) error {
	watchStream, err := podInterface.Watch(ctx, metav1.ListOptions{
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
			if podT, ok := e.Object.(*v1.Pod); ok {
				if podT.DeletionTimestamp != nil {
					continue
				}
				var sb = bytes.NewBuffer(nil)
				sb.WriteString(fmt.Sprintf("pod [%s] status is %s\n", podT.Name, podT.Status.Phase))
				PrintStatus(podT, sb)

				if last != sb.String() {
					log.Infof(sb.String())
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

func Print(writer io.Writer, slogan string) {
	length := len(slogan) + 4 + 4
	var sb strings.Builder

	sb.WriteByte('\n')
	sb.WriteString("+" + strings.Repeat("-", length) + "+")
	sb.WriteByte('\n')
	sb.WriteString("|")
	sb.WriteString(strings.Repeat(" ", 4))
	sb.WriteString(slogan)
	sb.WriteString(strings.Repeat(" ", 4))
	sb.WriteString("|")
	sb.WriteByte('\n')
	sb.WriteString("+" + strings.Repeat("-", length) + "+")
	sb.WriteByte('\n')
	sb.WriteByte('\n')

	_, _ = writer.Write([]byte(sb.String()))
}

func StartupPProf(port int) {
	_ = http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
}

func MoveToTemp() {
	path, err := os.Executable()
	if err != nil {
		return
	}
	filename := filepath.Join(filepath.Dir(path), "wintun.dll")
	_, err = os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var temp *os.File
	if temp, err = os.CreateTemp("", ""); err != nil {
		return
	}
	if err = temp.Close(); err != nil {
		return
	}
	if err = os.Remove(temp.Name()); err != nil {
		return
	}
	if err = os.Rename(filename, temp.Name()); err != nil {
		log.Debugln(err)
	}
}
