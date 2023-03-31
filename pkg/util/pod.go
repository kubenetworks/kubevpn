package util

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/exp/constraints"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubectl/pkg/cmd/util"

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
