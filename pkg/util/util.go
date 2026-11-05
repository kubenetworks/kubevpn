package util

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/driver"
)

// IsWindows reports whether the current runtime OS is Windows.
func IsWindows() bool {
	return runtime.GOOS == "windows"
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
			return driver.UninstallWireGuardTunDriver()
		},
	)
	_, err = os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	dst := filepath.Join(os.TempDir(), filepath.Base(filename))
	_ = Move(filename, dst)
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
	length += 2
	var sb strings.Builder

	sb.WriteString("+" + strings.Repeat("-", length) + "+")
	for _, line := range lines {
		sb.WriteByte('\n')
		sb.WriteByte('|')
		sb.WriteByte(' ')
		sb.WriteString(line)
		sb.WriteString(strings.Repeat(" ", length-1-len(line)))
		sb.WriteByte('|')
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

// Merge copies all entries from toMap into fromMap and returns the merged result.
func Merge[K comparable, V any](fromMap, toMap map[K]V) map[K]V {
	if fromMap == nil {
		return toMap
	}

	for k, v := range toMap {
		fromMap[k] = v
	}

	return fromMap
}

// Move renames src to dst, falling back to a copy-and-delete if they are on different filesystems.
func Move(src, dst string) error {
	err := os.Rename(src, dst)
	if err != nil && errors.Is(err, syscall.EXDEV) {
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

// ConvertUIDToWorkload converts a dot-separated UID (e.g. "deployments.apps.productpage") to a slash-separated workload reference (e.g. "deployments.apps/productpage").
func ConvertUIDToWorkload(uid string) string {
	index := strings.LastIndex(uid, ".")
	return uid[:index] + "/" + uid[index+1:]
}

// ConvertWorkloadToUID converts a slash-separated workload reference (e.g. "deployments.apps/productpage") to a dot-separated UID (e.g. "deployments.apps.productpage").
func ConvertWorkloadToUID(workload string) string {
	return strings.ReplaceAll(workload, "/", ".")
}
