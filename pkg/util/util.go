package util

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubectl/pkg/cmd/rollout"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"

	"github.com/wencaiwulue/kubevpn/v2/pkg/driver"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// IsWindows reports whether the current runtime OS is Windows.
func IsWindows() bool {
	return runtime.GOOS == "windows"
}

// RolloutStatus not use kubectl rollout options is this method can use context to cancel
func RolloutStatus(ctx1 context.Context, f cmdutil.Factory, ns, workloads string) (err error) {
	plog.GetLogger(ctx1).Infof("Checking rollout status for %s", workloads)
	defer func() {
		if err != nil {
			plog.G(ctx1).Errorf("Rollout status for %s failed: %s", workloads, err.Error())
			out := plog.GetLogger(ctx1).Logger.Out
			streams := genericiooptions.IOStreams{
				In:     os.Stdin,
				Out:    out,
				ErrOut: out,
			}
			o := rollout.NewRolloutUndoOptions(streams)
			cmd := &cobra.Command{}
			cmdutil.AddDryRunFlag(cmd)
			_ = o.Complete(f, cmd, []string{workloads})
			_ = o.Validate()
			_ = o.RunUndo()
		} else {
			plog.G(ctx1).Infof("Rollout successfully for %s", workloads)
		}
	}()
	client, _ := f.DynamicClient()
	r := f.NewBuilder().
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
	ctx, cancel := context.WithCancel(ctx1)
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
				plog.G(ctx).Info(strings.TrimSpace(status))
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

// WriterStringer combines io.Writer and fmt.Stringer for buffered output with string access.
type WriterStringer interface {
	io.Writer
	fmt.Stringer
}

// NewWriter returns a WriterStringer that stops buffering once the checker function returns true.
func NewWriter(checker func(log string) bool) WriterStringer {
	return &proxyWriter{Buffer: bytes.NewBuffer(make([]byte, 0)), checker: checker}
}

type proxyWriter struct {
	*bytes.Buffer
	checker func(log string) bool
	stop    bool
}

func (w *proxyWriter) Write(b []byte) (int, error) {
	if w.stop {
		return len(b), nil
	}

	write, err := w.Buffer.Write(b)
	if w.checker != nil {
		w.stop = w.checker(w.Buffer.String())
	}
	return write, err
}

func (w *proxyWriter) String() string {
	return w.Buffer.String()
}

func runWithRollingOutWithChecker(cmd *osexec.Cmd, checker func(log string) (stop bool)) (string, string, error) {
	stdoutBuf := NewWriter(checker)
	stderrBuf := NewWriter(checker)

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

// CleanExtensionLib removes the wintun.dll driver file on Windows after uninstalling the WireGuard TUN driver.
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

// Print writes the slogan wrapped in a banner box to the writer.
func Print(writer io.Writer, slogan string) {
	str := FormatBanner(slogan)
	_, _ = writer.Write([]byte(str))
}

// FormatBanner wraps the slogan text in an ASCII box border and returns the formatted string.
func FormatBanner(slogan string) string {
	scanner := bufio.NewScanner(strings.NewReader(slogan))
	var length int
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		length = max(length, len(line))
		lines = append(lines, line)
	}
	length = length + 1 + 1
	var sb strings.Builder

	sb.WriteString("+" + strings.Repeat("-", length) + "+")
	for _, line := range lines {
		sb.WriteByte('\n')
		sb.WriteString("|")
		sb.WriteString(strings.Repeat(" ", 1))
		sb.WriteString(line)
		sb.WriteString(strings.Repeat(" ", length-1-len(line)))
		sb.WriteString("|")
	}
	sb.WriteByte('\n')
	sb.WriteString("+" + strings.Repeat("-", length) + "+")

	return sb.String()
}

// StartupPProf starts an HTTP pprof server on localhost at the given port.
func StartupPProf(port int) {
	_ = http.ListenAndServe(fmt.Sprintf("localhost:%d", port), nil)
}

// StartupPProfForServer starts an HTTP pprof server listening on all interfaces at the given port.
func StartupPProfForServer(port int) {
	_ = http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
}

// Merge copies all entries from ToMap into fromMap and returns the merged result.
func Merge[K comparable, V any](fromMap, ToMap map[K]V) map[K]V {
	if fromMap == nil {
		return ToMap
	}

	for k, v := range ToMap {
		fromMap[k] = v
	}

	return fromMap
}

// Move renames src to dst, falling back to a copy-and-delete if they are on different filesystems.
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

// If returns t1 if b is true, otherwise t2 (a generic ternary helper).
func If[T any](b bool, t1, t2 T) T {
	if b {
		return t1
	}
	return t2
}

// ConvertUIDToWorkload
// deployments.apps.productpage --> deployments.apps/productpage
func ConvertUIDToWorkload(uid string) string {
	index := strings.LastIndex(uid, ".")
	return uid[:index] + "/" + uid[index+1:]
}

// ConvertWorkloadToUID
// deployments.apps/productpage --> deployments.apps.productpage
func ConvertWorkloadToUID(workload string) string {
	return strings.ReplaceAll(workload, "/", ".")
}
