package manager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func TestDetectManagerNamespace_SkipsPodWithoutConfigMap(t *testing.T) {
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
			writeJSON(w, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	factory := util.InitFactoryByBytes([]byte("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: "+server.URL+"\ncontexts:\n- name: test\n  context:\n    cluster: test\n    namespace: default\ncurrent-context: test\n"), "default")
	got, err := DetectManagerNamespace(context.Background(), factory, "default")
	if err != nil {
		t.Fatal(err)
	}
	if got != "kubevpn" {
		t.Fatalf("manager namespace = %q, want kubevpn (default has pod but no ConfigMap)", got)
	}
}
