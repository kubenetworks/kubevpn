package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
	"sigs.k8s.io/yaml"
)

func TestStatusSnapshot_RefreshesStaleManagerNamespace(t *testing.T) {
	managerPod := func(namespace string) corev1.Pod {
		return corev1.Pod{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
			ObjectMeta: metav1.ObjectMeta{Name: "kubevpn-traffic-manager", Namespace: namespace, Labels: map[string]string{"app": config.ConfigMapPodTrafficManager}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: config.ContainerSidecarVPN}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{
				Name: config.ContainerSidecarVPN, Ready: true,
			}}},
		}
	}
	writeJSON := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	newConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"}}
	virtuals := []*controlplane.Virtual{{Namespace: "default", UID: "services.reviews", ServiceName: "reviews", Rules: []*controlplane.Rule{{OwnerID: "owner-a", LocalTunIPv4: "198.18.0.1", PortMap: map[int32]string{9080: "8080"}}}}}
	data, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	newConfig.Data = map[string]string{config.KeyEnvoy: string(data)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/v1/namespaces/default/pods":
			writeJSON(w, &corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: []corev1.Pod{managerPod("default")}})
		case "/api/v1/namespaces/kubevpn/pods":
			writeJSON(w, &corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: []corev1.Pod{managerPod("kubevpn")}})
		case "/api/v1/namespaces/default/configmaps/kubevpn-traffic-manager":
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, &metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound})
		case "/api/v1/namespaces/kubevpn/configmaps/kubevpn-traffic-manager":
			writeJSON(w, newConfig)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	staleConfig := newConfig.DeepCopy()
	staleConfig.Namespace = "default"
	staleConfig.Data[config.KeyEnvoy] = strings.ReplaceAll(staleConfig.Data[config.KeyEnvoy], `"9080": "8080"`, `"9080": "9080"`)
	clientset := fake.NewSimpleClientset(staleConfig, newConfig)
	factory := util.InitFactoryByBytes([]byte("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: "+server.URL+"\ncontexts:\n- name: test\n  context:\n    cluster: test\n    namespace: default\ncurrent-context: test\n"), "default")
	connect := &ConnectOptions{ManagerNamespace: "default", WorkloadNamespace: "default", SessionBase: SessionBase{K8sClient: K8sClient{clientset: clientset, factory: factory}}}
	if err := connect.EnsureConfigMapSynced(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer connect.getConfigMapStore().Stop()
	clientset.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "default" {
			return true, nil, apierrors.NewNotFound(corev1.Resource("configmaps"), config.ConfigMapPodTrafficManager)
		}
		return false, nil, nil
	})
	snapshot, err := connect.StatusSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if connect.ManagerNamespace != "kubevpn" {
		t.Fatalf("manager namespace = %q, want kubevpn", connect.ManagerNamespace)
	}
	if !strings.Contains(snapshot.Envoy, `"9080": "8080"`) {
		t.Fatalf("status retained stale manager config: %s", snapshot.Envoy)
	}
}
