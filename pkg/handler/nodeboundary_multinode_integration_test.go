//go:build integration

package handler

import (
	"context"
	"encoding/json"
	"os/exec"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubectl/pkg/util/podutils"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

// TestIntegration_NodeBoundaryMultiNode verifies the paths that a single-node
// cluster cannot exercise: Service fallback with backends on two nodes, and a
// sync route whose source and resolved clone live on different nodes.
func TestIntegration_NodeBoundaryMultiNode(t *testing.T) {
	u := &ut{}
	u.init(t)
	nodes := readyNodeNames(t, u)
	if len(nodes) < 2 {
		t.Skip("node-boundary multi-node test requires at least two Ready nodes")
	}
	sourceNode, targetNode := nodes[0], nodes[1]
	t.Cleanup(func() { _ = exec.Command("kubevpn", "quit").Run() })

	t.Run("01-topology", func(t *testing.T) {
		pinDeploymentToNode(t, u, "authors", sourceNode)
		spreadReviewsAcrossNodes(t, u)
		waitForDeploymentPods(t, u, "app=authors", 1, []string{sourceNode})
		waitForDeploymentPods(t, u, "app=reviews", 2, nodes[:2])
	})
	u.kubevpnConnect(t)

	t.Run("02-service-cross-node-fallback", func(t *testing.T) {
		identity := capturePodUIDs(t, u, "app=reviews")
		targetPorts := captureServiceTargetPorts(t, u.clientset, u.namespace, "reviews")
		u.kubevpnProxyWithServiceMeshAndK8sServicePortMap(t)
		assertPodUIDsUnchanged(t, u, "app=reviews", identity)
		assertServiceTargetPortsUnchanged(t, u.clientset, u.namespace, "reviews", targetPorts)
		waitForServiceAttachmentNodes(t, u, "reviews", nodes[:2])

		// Both nodes have a Ready endpoint and a direct listener. Repeated requests
		// exercise kube-proxy's remote-endpoint choice as well as the local one;
		// unmatched traffic must always return to reviews, never recapture at the
		// remote node's KubeVPN DNAT rule.
		for range 4 {
			u.serviceMeshReviewsServiceIPPortMap(t)
		}
		u.kubevpnLeaveService(t)
	})

	t.Run("03-sync-clone-on-other-node", func(t *testing.T) {
		sourceUIDs := capturePodUIDs(t, u, "app=authors")
		sourceDeployment, err := u.clientset.AppsV1().Deployments(u.namespace).Get(context.Background(), "authors", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		endpoint := u.kubevpnSync(t, ctx, true)

		deployments, err := u.clientset.AppsV1().Deployments(u.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "origin-workload=authors"})
		if err != nil || len(deployments.Items) != 1 {
			t.Fatalf("list authors sync clone: count=%d err=%v", len(deployments.Items), err)
		}
		cloneName := deployments.Items[0].Name
		pinDeploymentToNode(t, u, cloneName, targetNode)
		clonePods := waitForDeploymentPods(t, u, "owner-ref="+cloneName, 1, []string{targetNode})
		cloneIP := clonePods[0].Status.PodIP
		u.checkContent(ctx, t, clonePods[0].Name, "authors", "/app/test/main.go")
		go u.execServer(ctx, t, clonePods[0].Name, "authors", "/app/test/main.go")
		waitForDerivedSyncEndpoint(t, u, "deployments.apps.authors", sourceNode, cloneName, cloneIP)
		assertPodUIDsUnchanged(t, u, "app=authors", sourceUIDs)
		sourceAfter, err := u.clientset.AppsV1().Deployments(u.namespace).Get(context.Background(), "authors", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if sourceAfter.Generation != sourceDeployment.Generation {
			t.Fatalf("sync clone migration changed source generation: before=%d after=%d", sourceDeployment.Generation, sourceAfter.Generation)
		}

		u.healthChecker(t, endpoint, nil, remoteSyncOrigin)
		u.healthChecker(t, endpoint, map[string]string{"env": "test"}, remoteSyncPod)
		u.kubevpnUnSync(t)
	})
}

func readyNodeNames(t *testing.T, u *ut) []string {
	t.Helper()
	list, err := u.clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range list.Items {
		for _, condition := range list.Items[i].Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				names = append(names, list.Items[i].Name)
			}
		}
	}
	sort.Strings(names)
	return names
}

func pinDeploymentToNode(t *testing.T, u *ut, name, node string) {
	t.Helper()
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"nodeSelector": map[string]string{corev1.LabelHostname: node},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = u.clientset.AppsV1().Deployments(u.namespace).Patch(context.Background(), name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("pin deployment %s to %s: %v", name, node, err)
	}
}

func spreadReviewsAcrossNodes(t *testing.T, u *ut) {
	t.Helper()
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"replicas": 2,
			"template": map[string]any{"spec": map[string]any{"affinity": map[string]any{
				"podAntiAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{
					"labelSelector": map[string]any{"matchLabels": map[string]string{"app": "reviews"}},
					"topologyKey":   corev1.LabelHostname,
				}}},
			}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = u.clientset.AppsV1().Deployments(u.namespace).Patch(context.Background(), "reviews", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("spread reviews across nodes: %v", err)
	}
}

func waitForDeploymentPods(t *testing.T, u *ut, selector string, count int, nodes []string) []corev1.Pod {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var result []corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		pods, err := u.clientset.CoreV1().Pods(u.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, nil
		}
		result = result[:0]
		seen := make(map[string]struct{})
		for i := range pods.Items {
			pod := &pods.Items[i]
			// Do not capture identities while an old ReplicaSet pod is still
			// terminating; its UID would disappear before the attach assertion.
			if pod.DeletionTimestamp != nil {
				return false, nil
			}
			if podutils.IsPodReady(pod) {
				result = append(result, *pod)
				seen[pod.Spec.NodeName] = struct{}{}
			}
		}
		if len(result) != count || len(seen) != len(nodes) {
			return false, nil
		}
		for _, node := range nodes {
			if _, ok := seen[node]; !ok {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("pods %s did not settle on nodes %v: %v", selector, nodes, err)
	}
	return result
}

func capturePodUIDs(t *testing.T, u *ut, selector string) map[types.UID]struct{} {
	t.Helper()
	pods, err := u.clientset.CoreV1().Pods(u.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("list pods %s: %v", selector, err)
	}
	result := make(map[types.UID]struct{}, len(pods.Items))
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning {
			result[pod.UID] = struct{}{}
		}
	}
	return result
}

func assertPodUIDsUnchanged(t *testing.T, u *ut, selector string, before map[types.UID]struct{}) {
	t.Helper()
	after := capturePodUIDs(t, u, selector)
	if len(after) != len(before) {
		t.Fatalf("pod identities changed: before=%v after=%v", before, after)
	}
	for uid := range before {
		if _, ok := after[uid]; !ok {
			t.Fatalf("attachment restarted pod %s", uid)
		}
	}
}

func waitForServiceAttachmentNodes(t *testing.T, u *ut, service string, nodes []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		cm, err := u.clientset.CoreV1().ConfigMaps(u.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		var attachments []controlplane.NodeAttachment
		if err = yaml.Unmarshal([]byte(cm.Data[config.KeyNodeAttachments]), &attachments); err != nil {
			return false, err
		}
		seen := make(map[string]struct{})
		for _, attachment := range attachments {
			if attachment.ServiceName == service && attachment.WorkloadUID == "services."+service {
				seen[attachment.NodeName] = struct{}{}
			}
		}
		for _, node := range nodes {
			if _, ok := seen[node]; !ok {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("Service %s did not attach on nodes %v: %v", service, nodes, err)
	}
}

func waitForDerivedSyncEndpoint(t *testing.T, u *ut, sourceUID, sourceNode, cloneName, cloneIP string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	nodeID := controlplane.NodeBoundaryNodeID(sourceUID, sourceNode)
	err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		cm, err := u.clientset.CoreV1().ConfigMaps(u.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		var virtuals []*controlplane.Virtual
		if err = yaml.Unmarshal([]byte(cm.Data[config.KeyAgentXDS]), &virtuals); err != nil {
			return false, err
		}
		for _, virtual := range virtuals {
			if virtual.UID != nodeID {
				continue
			}
			for _, rule := range virtual.Rules {
				if rule.SyncTarget != nil && rule.SyncTarget.UID == "deployments.apps."+cloneName && rule.LocalTunIPv4 == cloneIP {
					return true, nil
				}
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("sync route on %s did not resolve to clone %s (%s): %v", sourceNode, cloneName, cloneIP, err)
	}
}
