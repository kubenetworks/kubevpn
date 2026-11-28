package inject

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

func newNodeAgentReadyFixture(t *testing.T, listenerName string) (InjectOptions, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/listeners" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(listenerName))
	}))
	_, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	attachment := controlplane.NodeAttachment{
		Namespace: "default", WorkloadUID: "deployments.apps.reviews", NodeName: "node-a", PodIP: "10.244.0.5",
		NodeID: "deployments.apps.reviews.nodes.node-a", AgentListenerPort: int32(port - 5000),
		Ports: []controlplane.AgentOriginPort{{ContainerPort: 9080, Protocol: corev1.ProtocolTCP, AgentListenerPort: int32(port)}},
	}
	if listenerName == "" {
		listenerName = fmt.Sprintf("%s_%s_%d_%s", attachment.Namespace, attachment.NodeID, attachment.Ports[0].AgentListenerPort, attachment.Ports[0].Protocol)
	}
	data, err := yaml.Marshal([]controlplane.NodeAttachment{attachment})
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data:       map[string]string{config.KeyNodeAttachments: string(data)},
	}
	agentPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-a", Namespace: "kubevpn", Labels: map[string]string{"app": "kubevpn-nodeagent"}},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "127.0.0.1"},
	}
	clientset := fake.NewSimpleClientset(cm, agentPod)
	return InjectOptions{Clientset: clientset, ManagerNamespace: "kubevpn", NodeID: "deployments.apps.reviews"}, server
}

func TestWaitNodeAgentReady_RequiresDirectListener(t *testing.T) {
	opts, server := newNodeAgentReadyFixture(t, "")
	defer server.Close()
	if err := waitNodeAgentReady(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
}

func TestWaitNodeAgentReady_RejectsMissingListener(t *testing.T) {
	opts, server := newNodeAgentReadyFixture(t, "default_deployments.apps.reviews.nodes.node-a_20002_TCP")
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := waitNodeAgentReady(ctx, opts); err == nil {
		t.Fatal("expected missing listener error")
	}
}

func TestWaitNodeAgentReady_RejectsAttachmentWithoutAgentPod(t *testing.T) {
	opts, server := newNodeAgentReadyFixture(t, "")
	defer server.Close()
	clientset := opts.Clientset
	if err := clientset.CoreV1().Pods("kubevpn").Delete(context.Background(), "agent-a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := waitNodeAgentReady(ctx, opts); err == nil {
		t.Fatal("expected missing agent pod error")
	}
}

func TestWaitNodeAgentReady_RequiresResolvedSyncTarget(t *testing.T) {
	opts, server := newNodeAgentReadyFixture(t, "")
	defer server.Close()
	target := &controlplane.SyncTarget{UID: "deployments.apps.reviews-sync-a", Selector: "owner-ref=reviews-sync-a"}
	derived := &controlplane.Virtual{
		Namespace: "default", UID: "deployments.apps.reviews.nodes.node-a", AgentOrigins: []*controlplane.AgentOrigin{{PodUID: "source"}},
		Rules: []*controlplane.Rule{{OwnerID: "owner-sync", SyncTarget: target}},
	}
	data, err := yaml.Marshal([]*controlplane.Virtual{derived})
	if err != nil {
		t.Fatal(err)
	}
	cm, err := opts.Clientset.CoreV1().ConfigMaps("kubevpn").Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[config.KeyAgentXDS] = string(data)
	if _, err := opts.Clientset.CoreV1().ConfigMaps("kubevpn").Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	opts.OwnerID = "owner-sync"
	opts.SyncTarget = target
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := waitNodeAgentReady(ctx, opts); err == nil {
		t.Fatal("expected unresolved sync target error")
	}
	derived.Rules[0].LocalTunIPv4 = "10.244.1.10"
	data, err = yaml.Marshal([]*controlplane.Virtual{derived})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[config.KeyAgentXDS] = string(data)
	if _, err := opts.Clientset.CoreV1().ConfigMaps("kubevpn").Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := waitNodeAgentReady(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
}

func TestNodeAgentListenerName(t *testing.T) {
	attachment := controlplane.NodeAttachment{Namespace: "default", NodeID: "deployments.apps.reviews.nodes.node-a"}
	port := controlplane.AgentOriginPort{AgentListenerPort: 20001, Protocol: corev1.ProtocolTCP}
	if got := nodeAgentListenerName(attachment, port); got != fmt.Sprintf("%s_%s_%d_%s", attachment.Namespace, attachment.NodeID, port.AgentListenerPort, port.Protocol) {
		t.Fatalf("unexpected listener name %q", got)
	}
}
