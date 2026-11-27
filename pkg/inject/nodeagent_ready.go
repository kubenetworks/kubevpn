package inject

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

// waitNodeAgentReady waits until every direct listener for this workload is
// actually active, not merely present in the desired ConfigMap. Returning before
// this point lets clients establish keep-alive connections to the remote app.
func waitNodeAgentReady(ctx context.Context, opts InjectOptions) error {
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		cm, err := opts.Clientset.CoreV1().ConfigMaps(opts.ManagerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		var attachments []controlplane.NodeAttachment
		if data := cm.Data[config.KeyNodeAttachments]; data != "" {
			if err := yaml.Unmarshal([]byte(data), &attachments); err != nil {
				return false, fmt.Errorf("parse node attachments: %w", err)
			}
		}
		pods, err := opts.Clientset.CoreV1().Pods(opts.ManagerNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app=kubevpn-nodeagent"})
		if err != nil {
			return false, nil
		}
		agentByNode := make(map[string]string)
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
				agentByNode[pod.Spec.NodeName] = pod.Status.PodIP
			}
		}
		checked := false
		for _, attachment := range attachments {
			if attachment.WorkloadUID != opts.NodeID {
				continue
			}
			agentIP := agentByNode[attachment.NodeName]
			if agentIP == "" {
				return false, nil
			}
			for _, port := range attachment.Ports {
				checked = true
				if !nodeAgentListenerReady(ctx, agentIP, attachment.AgentListenerPort+5000, nodeAgentListenerName(attachment, port)) {
					return false, nil
				}
			}
		}
		return checked, nil
	})
	if err != nil {
		return fmt.Errorf("wait for node-agent listeners: %w", err)
	}
	return nil
}

func nodeAgentListenerName(attachment controlplane.NodeAttachment, port controlplane.AgentOriginPort) string {
	return fmt.Sprintf("%s_%s_%d_%s", attachment.Namespace, attachment.NodeID, port.AgentListenerPort, port.Protocol)
}

func nodeAgentListenerReady(ctx context.Context, agentIP string, adminPort int32, listenerName string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/listeners", agentIP, adminPort), nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return false
	}
	return strings.Contains(string(body), listenerName)
}
