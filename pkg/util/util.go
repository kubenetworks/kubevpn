package util

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
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

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/driver"
)

func GetAvailableUDPPortOrDie() (int, error) {
	address, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:0", "localhost"))
	if err != nil {
		return 0, err
	}
	listener, err := net.ListenUDP("udp", address)
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.LocalAddr().(*net.UDPAddr).Port, nil
}

func GetAvailableTCPPortOrDie() (int, error) {
	address, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:0", "localhost"))
	if err != nil {
		return 0, err
	}
	listener, err := net.ListenTCP("tcp", address)
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func IsWindows() bool {
	return runtime.GOOS == "windows"
}

func BytesToInt(b []byte) uint32 {
	buffer := bytes.NewBuffer(b)
	var u uint32
	if err := binary.Read(buffer, binary.BigEndian, &u); err != nil {
		log.Warn(err)
	}
	return u
}

func RolloutStatus(ctx1 context.Context, factory cmdutil.Factory, namespace, workloads string, timeout time.Duration) (err error) {
	log.Infof("rollout status for %s", workloads)
	defer func() {
		if err != nil {
			log.Errorf("rollout status for %s failed: %s", workloads, err.Error())
		} else {
			log.Infof("rollout status for %s successfully", workloads)
		}
	}()
	client, _ := factory.DynamicClient()
	r := factory.NewBuilder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		NamespaceParam(namespace).DefaultNamespace().
		ResourceTypeOrNameArgs(true, workloads).
		SingleResourceType().
		Latest().
		Do()
	err = r.Err()
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
				log.Info(strings.TrimSpace(status))
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
	if err := cmd.Wait(); err != nil {
		return "", "", err
	}
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
	ticker := time.NewTicker(time.Second * 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait port %v to be free timeout", port)
		case <-ticker.C:
			if !IsPortListening(port) {
				log.Infoln(fmt.Sprintf("port %v are free", port))
				return nil
			}
		}
	}
}

func IsPortListening(port int) bool {
	listener, err := net.Listen("tcp4", net.JoinHostPort("localhost", strconv.Itoa(port)))
	if err != nil {
		return true
	} else {
		_ = listener.Close()
		return false
	}
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

func Print(writer io.Writer, slogan string) {
	length := len(slogan) + 4 + 4
	var sb strings.Builder

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

	_, _ = writer.Write([]byte(sb.String()))
}

func StartupPProf(port int) {
	_ = http.ListenAndServe(fmt.Sprintf("localhost:%d", port), nil)
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

func Merge[K comparable, V any](fromMap, ToMap map[K]V) map[K]V {
	if fromMap == nil {
		return ToMap
	}

	for k, v := range ToMap {
		fromMap[k] = v
	}

	return fromMap
}
