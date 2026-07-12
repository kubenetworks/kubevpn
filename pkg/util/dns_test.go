package util

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// GetDNSServiceIPFromPod must return the manager-published resolv.conf from the
// ConfigMap cache WITHOUT exec-ing into the pod. Passing a nil rest.Config proves the
// exec fallback is not reached (it would panic/err); a cache hit returns first.
func TestGetDNSServiceIPFromPod_ReadsConfigMapCache(t *testing.T) {
	const ns = "default"
	cs := fake.NewSimpleClientset(&v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: ns},
		Data: map[string]string{
			config.KeyClusterDNS: "nameserver 10.96.0.10\nsearch default.svc.cluster.local svc.cluster.local cluster.local\noptions ndots:5\n",
		},
	})

	rc, err := GetDNSServiceIPFromPod(context.Background(), cs, nil, "manager-pod", ns)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rc.Servers) != 1 || rc.Servers[0] != "10.96.0.10" {
		t.Fatalf("servers: got %v, want [10.96.0.10]", rc.Servers)
	}
	if rc.Port != "53" {
		t.Fatalf("port: got %q, want 53", rc.Port)
	}
	found := false
	for _, s := range rc.Search {
		if s == "svc.cluster.local" {
			found = true
		}
	}
	if !found {
		t.Fatalf("search domains missing svc.cluster.local: %v", rc.Search)
	}
}
