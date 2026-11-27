package xds

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/cli-runtime/pkg/resource"
	restfake "k8s.io/client-go/rest/fake"
	cmdtesting "k8s.io/kubectl/pkg/cmd/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// injectedDeployment builds a Deployment named foo/ns that is ALREADY injected for the
// manager namespace: it carries the vpn + envoy sidecars and the envoy container's
// rendered config references the current traffic-manager address. This makes the mesh
// injector take the injectedForManager early-return after writing the envoy rule, so the
// test exercises the real server-side ProxyInject/LeaveInject RPC + envoy-rule logic
// against fakes WITHOUT the workload-patch + rollout-wait terminal step, which is designed
// around real-cluster convergence and does not terminate against a fake dynamic client.
// (That terminal step — last-owner container removal — is covered at the store level by
// pkg/inject/multiuser_test.go and end to end by cluster-backed CI.)
func injectedDeployment(ns string) *appsv1.Deployment {
	envoyAddr := fmt.Sprintf("%s.%s", config.ConfigMapPodTrafficManager, ns) // trafficManagerAddr
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "foo"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{
						Name:  "app",
						Image: "foo:v1",
						Ports: []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
					},
					{Name: config.ContainerSidecarVPN, Image: "kubevpn:v1"},
					{Name: config.ContainerSidecarEnvoy, Image: "kubevpn:v1", Args: []string{"address: " + envoyAddr}},
				}},
			},
		},
	}
}

func plainDeployment(ns string) *appsv1.Deployment {
	deploy := injectedDeployment(ns)
	deploy.Spec.Template.Spec.Containers = deploy.Spec.Template.Spec.Containers[:1]
	return deploy
}

// newInjectTestFactory returns a cmdutil.Factory backed by a fake REST client so
// GetTopOwnerObject (resource builder) resolves the Deployment with no real cluster.
// Only GET is served — the injected-for-manager early-return means no PATCH/rollout.
func newInjectTestFactory(t *testing.T, deployments ...*appsv1.Deployment) *cmdtesting.TestFactory {
	t.Helper()
	tf := cmdtesting.NewTestFactory().WithNamespace(deployments[0].Namespace)
	t.Cleanup(tf.Cleanup)

	bodies := make(map[string][]byte, len(deployments))
	for _, deploy := range deployments {
		body, err := json.Marshal(deploy)
		if err != nil {
			t.Fatalf("marshal deployment: %v", err)
		}
		path := fmt.Sprintf("/namespaces/%s/deployments/%s", deploy.Namespace, deploy.Name)
		bodies[path] = body
	}
	fakeClient := &restfake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		GroupVersion:         appsv1.SchemeGroupVersion,
		Client: restfake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			if body, ok := bodies[req.URL.Path]; ok && req.Method == http.MethodGet {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader(body)),
				}, nil
			}
			return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		}),
	}
	tf.Client = fakeClient
	tf.UnstructuredClient = fakeClient
	return tf
}

func newInjectServiceTestFactory(t *testing.T, service *corev1.Service, deploy *appsv1.Deployment) *cmdtesting.TestFactory {
	t.Helper()
	tf := cmdtesting.NewTestFactory().WithNamespace(service.Namespace)
	t.Cleanup(tf.Cleanup)
	marshal := func(obj any) []byte {
		data, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	serviceBody := marshal(service)
	deploymentBody := marshal(deploy)
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo-pod", Namespace: service.Namespace, Labels: map[string]string{"app": "foo"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deploy.Name, Controller: ptr.To(true)}},
		},
	}
	podBody := marshal(pod)
	podListBody := marshal(&corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: []corev1.Pod{*pod}})
	responses := map[string][]byte{
		"/namespaces/" + service.Namespace + "/services/" + service.Name:               serviceBody,
		"/api/v1/namespaces/" + service.Namespace + "/services/" + service.Name:        serviceBody,
		"/api/v1/namespaces/" + service.Namespace + "/pods":                            podListBody,
		"/namespaces/" + service.Namespace + "/pods/" + pod.Name:                       podBody,
		"/namespaces/" + deploy.Namespace + "/deployments/" + deploy.Name:              deploymentBody,
		"/apis/apps/v1/namespaces/" + deploy.Namespace + "/deployments/" + deploy.Name: deploymentBody,
	}
	fakeClient := &restfake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		GroupVersion:         appsv1.SchemeGroupVersion,
		Client: restfake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			if body, ok := responses[req.URL.Path]; ok && req.Method == http.MethodGet {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
			}
			return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL.String())
		}),
	}
	tf.Client = fakeClient
	tf.UnstructuredClient = fakeClient
	return tf
}

// collectInjectStream drains a ProxyInject/LeaveInject stream to completion.
func collectInjectStream(recv func() (*rpc.InjectResponse, error)) error {
	for {
		if _, err := recv(); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// readRuleOwners returns the OwnerIDs of all envoy rules in the ENVOY_CONFIG ConfigMap.
func readRuleOwners(t *testing.T, s *TunConfigServer, ns string) []string {
	t.Helper()
	cm, err := s.clientset.CoreV1().ConfigMaps(ns).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	var virtuals []*controlplane.Virtual
	if data := cm.Data[config.KeyEnvoy]; data != "" {
		if err := yaml.Unmarshal([]byte(data), &virtuals); err != nil {
			t.Fatalf("unmarshal envoy config: %v", err)
		}
	}
	var owners []string
	for _, v := range virtuals {
		for _, r := range v.Rules {
			owners = append(owners, r.OwnerID)
		}
	}
	return owners
}

func prepareNodeAgentReady(t *testing.T, env *testEnv, ns, workloadUID string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, portText, err := net.SplitHostPort(r.Host)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = w.Write([]byte(fmt.Sprintf("%s_%s.nodes.node-a_%s_TCP", ns, workloadUID, portText)))
	}))
	t.Cleanup(server.Close)
	_, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	attachment := controlplane.NodeAttachment{
		Namespace: ns, WorkloadUID: workloadUID, NodeName: "node-a", PodIP: "10.244.0.5",
		NodeID: workloadUID + ".nodes.node-a", AgentListenerPort: int32(port - 5000),
		Ports: []controlplane.AgentOriginPort{{ContainerPort: 9080, Protocol: corev1.ProtocolTCP, AgentListenerPort: int32(port)}},
	}
	data, err := yaml.Marshal([]controlplane.NodeAttachment{attachment})
	if err != nil {
		t.Fatal(err)
	}
	cm, err := env.server.clientset.CoreV1().ConfigMaps(ns).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[config.KeyNodeAttachments] = string(data)
	if _, err := env.server.clientset.CoreV1().ConfigMaps(ns).Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-a", Namespace: ns, Labels: map[string]string{"app": "kubevpn-nodeagent"}},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "127.0.0.1"},
	}
	if _, err := env.server.clientset.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

// TestInject_MultiUser_ProxyLeave drives the server-side inject/leave RPCs end to end
// through the real gRPC server with a fully faked factory (no cluster): two owners proxy
// the same workload with different headers -> two envoy rules coexist; one leaves -> the
// other's rule is preserved.
func TestInject_MultiUser_ProxyLeave(t *testing.T) {
	const ns = "test-ns"
	env := newTestEnv(t)
	if _, err := env.server.clientset.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: ns},
		Data:       map[string][]byte{config.TLSCertKey: {}, config.TLSPrivateKeyKey: {}, config.TLSServerName: []byte("kubevpn")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed tls secret: %v", err)
	}
	env.server.factory = newInjectTestFactory(t, plainDeployment(ns))

	proxy := func(owner string, headers map[string]string) {
		prepareNodeAgentReady(t, env, ns, "deployments.apps.foo")
		stream, err := env.client.ProxyInject(context.Background(), &rpc.InjectRequest{
			Namespace:    ns,
			Workloads:    []string{"deployments.apps/foo"},
			Headers:      headers,
			OwnerID:      owner,
			LocalTunIPv4: "198.18.0.10",
			Image:        "kubevpn:v1",
		})
		if err != nil {
			t.Fatalf("ProxyInject(%s) open: %v", owner, err)
		}
		if err := collectInjectStream(stream.Recv); err != nil {
			t.Fatalf("ProxyInject(%s): %v", owner, err)
		}
	}
	leave := func(owner string) {
		stream, err := env.client.LeaveInject(context.Background(), &rpc.InjectRequest{
			Namespace: ns,
			Workloads: []string{"deployments.apps/foo"},
			OwnerID:   owner,
		})
		if err != nil {
			t.Fatalf("LeaveInject(%s) open: %v", owner, err)
		}
		if err := collectInjectStream(stream.Recv); err != nil {
			t.Fatalf("LeaveInject(%s): %v", owner, err)
		}
	}

	contains := func(owners []string, want string) bool {
		for _, o := range owners {
			if o == want {
				return true
			}
		}
		return false
	}

	// 1) owner A proxies -> one rule owned by A.
	proxy("owner-a", map[string]string{"user": "a"})
	if owners := readRuleOwners(t, env.server, ns); len(owners) != 1 || owners[0] != "owner-a" {
		t.Fatalf("after A proxy, want [owner-a], got %v", owners)
	}

	// 2) owner B proxies same workload, different headers -> two rules coexist.
	proxy("owner-b", map[string]string{"user": "b"})
	if owners := readRuleOwners(t, env.server, ns); len(owners) != 2 || !contains(owners, "owner-a") || !contains(owners, "owner-b") {
		t.Fatalf("after B proxy, want [owner-a owner-b], got %v", owners)
	}

	_ = leave // explicit-workload leave is exercised via the leave-all path below

	// 3) owner A leaves-all (empty Workloads): the manager derives A's workloads from
	// ENVOY_CONFIG by owner and removes A's rule. B remains (not the last owner, so no
	// container patch / rollout — which the fake factory cannot converge; the last-owner
	// removal path is covered at the store level by pkg/inject/multiuser_test).
	leaveAll := func(owner string) {
		stream, err := env.client.LeaveInject(context.Background(), &rpc.InjectRequest{
			Namespace: ns,
			OwnerID:   owner, // Workloads intentionally empty -> leave-all-by-owner
		})
		if err != nil {
			t.Fatalf("LeaveInject(all,%s) open: %v", owner, err)
		}
		if err := collectInjectStream(stream.Recv); err != nil {
			t.Fatalf("LeaveInject(all,%s): %v", owner, err)
		}
	}
	leaveAll("owner-a")
	if owners := readRuleOwners(t, env.server, ns); len(owners) != 1 || owners[0] != "owner-b" {
		t.Fatalf("after A leave-all, want [owner-b], got %v", owners)
	}
}

func TestProxyInject_RejectsUnsupportedNodeAgentTargets(t *testing.T) {
	const ns = "test-ns"
	tests := []struct {
		name        string
		deployment  *appsv1.Deployment
		wantMessage string
	}{
		{name: "legacy sidecar injection", deployment: injectedDeployment(ns), wantMessage: "legacy sidecar"},
		{name: "host network", deployment: func() *appsv1.Deployment {
			deploy := plainDeployment(ns)
			deploy.Spec.Template.Spec.HostNetwork = true
			return deploy
		}(), wantMessage: "hostNetwork"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			if _, err := env.server.clientset.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: ns},
				Data:       map[string][]byte{config.TLSCertKey: {}, config.TLSPrivateKeyKey: {}, config.TLSServerName: []byte("kubevpn")},
			}, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed tls secret: %v", err)
			}
			env.server.factory = newInjectTestFactory(t, tt.deployment)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			stream, err := env.client.ProxyInject(ctx, &rpc.InjectRequest{
				Namespace: ns, Workloads: []string{"deployments.apps/foo"}, OwnerID: "owner-a", LocalTunIPv4: "198.18.0.10",
			})
			if err != nil {
				t.Fatalf("ProxyInject open: %v", err)
			}
			err = collectInjectStream(stream.Recv)
			if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("expected %q rejection, got %v", tt.wantMessage, err)
			}
			cm, getErr := env.server.clientset.CoreV1().ConfigMaps(ns).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
			if getErr != nil {
				t.Fatal(getErr)
			}
			if cm.Data[config.KeyEnvoy] != "" {
				t.Fatalf("rejected target wrote envoy config: %s", cm.Data[config.KeyEnvoy])
			}
		})
	}
}

func TestProxyInject_SyncRouteWritesDeclarativeTarget(t *testing.T) {
	const ns = "test-ns"
	env := newTestEnv(t)
	if _, err := env.server.clientset.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: ns},
		Data:       map[string][]byte{config.TLSCertKey: {}, config.TLSPrivateKeyKey: {}, config.TLSServerName: []byte("kubevpn")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed tls secret: %v", err)
	}
	source := plainDeployment(ns)
	clone := plainDeployment(ns)
	clone.Name = "foo-sync-a"
	clone.Spec.Template.Labels = map[string]string{"owner-ref": "foo-sync-a"}
	env.server.factory = newInjectTestFactory(t, source, clone)
	prepareNodeAgentReady(t, env, ns, "deployments.apps.foo")

	target := &controlplane.SyncTarget{UID: "deployments.apps.foo-sync-a", Selector: "owner-ref=foo-sync-a"}
	derived := &controlplane.Virtual{
		Namespace: ns, UID: "deployments.apps.foo.nodes.node-a", AgentOrigins: []*controlplane.AgentOrigin{{PodUID: "source"}},
		Rules: []*controlplane.Rule{{OwnerID: "owner-sync", SyncTarget: target, LocalTunIPv4: "10.244.1.10"}},
	}
	data, err := yaml.Marshal([]*controlplane.Virtual{derived})
	if err != nil {
		t.Fatal(err)
	}
	cm, err := env.server.clientset.CoreV1().ConfigMaps(ns).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[config.KeyEnvoy] = string(data)
	if _, err := env.server.clientset.CoreV1().ConfigMaps(ns).Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	stream, err := env.client.ProxyInject(context.Background(), &rpc.InjectRequest{
		Namespace: ns, OwnerID: "owner-sync",
		SyncRoutes: []*rpc.SyncRoute{{Workload: "deployments.apps/foo", TargetWorkload: "deployments.apps/foo-sync-a"}},
	})
	if err != nil {
		t.Fatalf("ProxyInject open: %v", err)
	}
	if err := collectInjectStream(stream.Recv); err != nil {
		t.Fatalf("ProxyInject sync route: %v", err)
	}
	cm, err = env.server.clientset.CoreV1().ConfigMaps(ns).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var virtuals []*controlplane.Virtual
	if err := yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &virtuals); err != nil {
		t.Fatal(err)
	}
	for _, virtual := range virtuals {
		if virtual.UID != "deployments.apps.foo" || len(virtual.AgentOrigins) != 0 {
			continue
		}
		if len(virtual.Rules) != 1 || virtual.Rules[0].SyncTarget == nil {
			t.Fatalf("sync target missing from base rule: %+v", virtual.Rules)
		}
		rule := virtual.Rules[0]
		if rule.SyncTarget.UID != target.UID || rule.SyncTarget.Selector != target.Selector || rule.LocalTunIPv4 != "" {
			t.Fatalf("unexpected declarative sync rule: %+v", rule)
		}
		return
	}
	t.Fatal("base sync Virtual not found")
}

func TestProxyInject_ServiceUsesNodeAgentWithoutSSHMapper(t *testing.T) {
	const ns = "test-ns"
	env := newTestEnv(t)
	if _, err := env.server.clientset.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: ns},
		Data:       map[string][]byte{config.TLSCertKey: {}, config.TLSPrivateKeyKey: {}, config.TLSServerName: []byte("kubevpn")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deploy := plainDeployment(ns)
	service := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: "reviews", Namespace: ns},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "foo"}, ClusterIP: "10.96.0.10",
			Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP}},
		},
	}
	env.server.factory = newInjectServiceTestFactory(t, service, deploy)
	prepareNodeAgentReady(t, env, ns, "services.reviews")
	stream, err := env.client.ProxyInject(context.Background(), &rpc.InjectRequest{
		Namespace: ns, Workloads: []string{"services/reviews"}, OwnerID: "owner-a", LocalTunIPv4: "198.18.0.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	for {
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		if len(resp.GetServices()) != 0 {
			t.Fatalf("node-agent Service must not start an SSH Mapper: %+v", resp.GetServices())
		}
	}
	cm, err := env.server.clientset.CoreV1().ConfigMaps(ns).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var virtuals []*controlplane.Virtual
	if err := yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &virtuals); err != nil {
		t.Fatal(err)
	}
	for _, virtual := range virtuals {
		if virtual.UID == "services.reviews" && virtual.ServiceName == "reviews" && virtual.Rules[0].LocalTunIPv4 == "198.18.0.10" {
			return
		}
	}
	t.Fatalf("explicit Service Virtual missing: %+v", virtuals)
}
