package workload

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
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

// PodStatusSummary returns a single-line, human-readable reason a pod is not yet
// ready — the waiting/terminated reason per container, or the unmet scheduling
// condition (e.g. "PodScheduled=Unschedulable"). Suitable for a live spinner
// line; the full detail (with messages) is available via PrintStatus.
func PodStatusSummary(pod *corev1.Pod) string {
	var parts []string
	for _, s := range pod.Status.ContainerStatuses {
		switch {
		case s.State.Waiting != nil:
			parts = append(parts, fmt.Sprintf("%s=%s", s.Name, s.State.Waiting.Reason))
		case s.State.Terminated != nil:
			parts = append(parts, fmt.Sprintf("%s=%s", s.Name, s.State.Terminated.Reason))
		case s.State.Running != nil:
			parts = append(parts, fmt.Sprintf("%s=Running", s.Name))
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, ", ")
	}
	for _, c := range pod.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			if c.Reason != "" {
				return fmt.Sprintf("%s=%s", c.Type, c.Reason)
			}
			return string(c.Type)
		}
	}
	if pod.Status.Phase != "" {
		return string(pod.Status.Phase)
	}
	return "pending"
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

// WaitPodReady blocks until a pod matching labelSelector is ready. When stepName
// is non-empty the wait is shown as a single in-place spinner line that updates
// with a one-line pod status ("Waiting for <stepName> pod (<summary>)"); when
// empty (callers that own their own step, e.g. sync, and tests) the status is
// logged at Debug instead. The full multi-line status table is kept only for the
// timeout error, so the reason survives without --debug.
func WaitPodReady(ctx context.Context, clientset coreclient.PodInterface, labelSelector, stepName string) error {
	const (
		podReadyTimeout      = 60 * time.Minute
		podReadyPollInterval = 3 * time.Second
	)
	var isPodReady bool
	var lastSummary, lastDetail string
	if stepName != "" {
		plog.StepStart(ctx, fmt.Sprintf("Waiting for %s pod", stepName))
	}
	ctx2, cancelFunc := context.WithTimeout(ctx, podReadyTimeout)
	defer cancelFunc()
	wait.UntilWithContext(ctx2, func(ctx context.Context) {
		podList, err := clientset.List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return
		}
		if len(podList.Items) == 0 {
			return
		}
		for _, pod := range podList.Items {
			if podutils.IsPodReady(&pod) && pod.DeletionTimestamp == nil {
				isPodReady = true
				cancelFunc()
				return
			}
			// Only react when the one-line summary changes, to avoid needless
			// repaints / log lines.
			summary := PodStatusSummary(&pod)
			if summary == lastSummary {
				continue
			}
			lastSummary = summary
			var sb bytes.Buffer
			PrintStatus(&pod, &sb)
			lastDetail = sb.String()
			if stepName != "" {
				plog.StepStart(ctx, fmt.Sprintf("Waiting for %s pod (%s)", stepName, summary))
			} else {
				plog.G(ctx).Debugf("%s", summary)
			}
		}
	}, podReadyPollInterval)
	if !isPodReady {
		cause := config.ErrTrafficManagerTimeout
		if strings.Contains(lastDetail, "ImagePullBackOff") || strings.Contains(lastDetail, "ErrImagePull") {
			cause = config.ErrImagePull
		}
		if lastDetail != "" {
			return fmt.Errorf("wait for pod ready timeout, last status:\n%s: %w", lastDetail, cause)
		}
		return fmt.Errorf("wait for pod ready timeout: %w", cause)
	}
	return nil
}
