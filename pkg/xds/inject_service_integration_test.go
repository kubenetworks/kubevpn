package xds

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// newInjectTestFactory returns a cmdutil.Factory backed by a fake REST client so
// GetTopOwnerObject (resource builder) resolves the Deployment with no real cluster.
// Only GET is served — the injected-for-manager early-return means no PATCH/rollout.
func newInjectTestFactory(t *testing.T, deploy *appsv1.Deployment) *cmdtesting.TestFactory {
	t.Helper()
	tf := cmdtesting.NewTestFactory().WithNamespace(deploy.Namespace)
	t.Cleanup(tf.Cleanup)

	body, err := json.Marshal(deploy)
	if err != nil {
		t.Fatalf("marshal deployment: %v", err)
	}
	path := fmt.Sprintf("/namespaces/%s/deployments/%s", deploy.Namespace, deploy.Name)
	fakeClient := &restfake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		GroupVersion:         appsv1.SchemeGroupVersion,
		Client: restfake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == path && req.Method == http.MethodGet {
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
	env.server.factory = newInjectTestFactory(t, injectedDeployment(ns))

	proxy := func(owner string, headers map[string]string) {
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

	// 3) owner A leaves (not the last owner) -> only B remains, no workload patch needed.
	leave("owner-a")
	if owners := readRuleOwners(t, env.server, ns); len(owners) != 1 || owners[0] != "owner-b" {
		t.Fatalf("after A leave, want [owner-b], got %v", owners)
	}
}
