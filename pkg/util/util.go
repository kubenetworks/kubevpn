package util

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

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

func IsWindows() bool {
	return runtime.GOOS == "windows"
}

func RolloutStatus(ctx1 context.Context, factory cmdutil.Factory, ns, workloads string, timeout time.Duration) (err error) {
	log.Infof("Checking rollout status for %s", workloads)
	defer func() {
		if err != nil {
			log.Errorf("Rollout status for %s failed: %s", workloads, err.Error())
		} else {
			log.Infof("Rollout successfully for %s", workloads)
		}
	}()
	client, _ := factory.DynamicClient()
	r := factory.NewBuilder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		NamespaceParam(ns).DefaultNamespace().
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
			return client.Resource(info.Mapping.Resource).Namespace(info.Namespace).List(ctx1, options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			options.FieldSelector = fieldSelector
			return client.Resource(info.Mapping.Resource).Namespace(info.Namespace).Watch(ctx1, options)
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
				// Quit waiting if the rollout is done
				if done {
					return true, nil
				}
				log.Info(strings.TrimSpace(status))
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

func GetTlsDomain(ns string) string {
	return config.ConfigMapPodTrafficManager + "." + ns + "." + "svc"
}

func RemoveLargerOverlappingCIDRs(cidrNets []*net.IPNet) []*net.IPNet {
	sort.Slice(cidrNets, func(i, j int) bool {
		onesI, _ := cidrNets[i].Mask.Size()
		onesJ, _ := cidrNets[j].Mask.Size()
		return onesI > onesJ
	})

	var cidrsOverlap = func(cidr1, cidr2 *net.IPNet) bool {
		return cidr1.Contains(cidr2.IP) || cidr2.Contains(cidr1.IP)
	}

	var result []*net.IPNet
	skipped := make(map[int]bool)

	for i := range cidrNets {
		if skipped[i] {
			continue
		}
		for j := i + 1; j < len(cidrNets); j++ {
			if cidrsOverlap(cidrNets[i], cidrNets[j]) {
				skipped[j] = true
			}
		}
		result = append(result, cidrNets[i])
	}
	return result
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
			err := driver.UninstallWireGuardTunDriver()
			return err
		},
	)
	_, err = os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	dst := filepath.Join(os.TempDir(), filepath.Base(filename))
	_ = Move(filename, dst)
}

func Print(writer io.Writer, slogan string) {
	length := len(slogan) + 1 + 1
	var sb strings.Builder

	sb.WriteString("+" + strings.Repeat("-", length) + "+")
	sb.WriteByte('\n')
	sb.WriteString("|")
	sb.WriteString(strings.Repeat(" ", 1))
	sb.WriteString(slogan)
	sb.WriteString(strings.Repeat(" ", 1))
	sb.WriteString("|")
	sb.WriteByte('\n')
	sb.WriteString("+" + strings.Repeat("-", length) + "+")
	sb.WriteByte('\n')

	_, _ = writer.Write([]byte(sb.String()))
}

func StartupPProf(port int) {
	_ = http.ListenAndServe(fmt.Sprintf("localhost:%d", port), nil)
}

func StartupPProfForServer(port int) {
	_ = http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
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

func Move(src, dst string) error {
	err := os.Rename(src, dst)
	if err != nil && errors.Is(err.(*os.LinkError).Err.(syscall.Errno), syscall.EXDEV) {
		return move(src, dst)
	}
	return err
}

func move(src, dst string) (e error) {
	defer func() {
		if e != nil {
			_ = os.Remove(dst)
		}
	}()

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return err
	}

	var fi os.FileInfo
	fi, err = os.Stat(src)
	if err != nil {
		return err
	}

	err = os.Chmod(dst, fi.Mode())
	if err != nil {
		return err
	}
	return os.Remove(src)
}

func If[T any](b bool, t1, t2 T) T {
	if b {
		return t1
	}
	return t2
}

// ConvertUidToWorkload
// deployments.apps.productpage --> deployments.apps/productpage
func ConvertUidToWorkload(uid string) string {
	index := strings.LastIndex(uid, ".")
	return uid[:index] + "/" + uid[index+1:]
}

// ConvertWorkloadToUid
// deployments.apps/productpage --> deployments.apps.productpage
func ConvertWorkloadToUid(workload string) string {
	return strings.ReplaceAll(workload, "/", ".")
}
