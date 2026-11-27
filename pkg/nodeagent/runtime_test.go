package nodeagent

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestBuildTunnelCommand_SetsManagerServiceAndAgent(t *testing.T) {
	const managerService = "kubevpn-traffic-manager.kubevpn"
	const managerAddr = managerService + ":10801"
	cmd := buildTunnelCommand(context.Background(), "kubevpn", managerService, "node-a")
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "tcp://"+managerAddr) || !strings.Contains(args, "agent=node-a") {
		t.Fatalf("tunnel URL does not contain manager address and node identity: %s", args)
	}
	wantEnv := "TrafficManagerService=" + managerService
	for _, env := range cmd.Env {
		if env == wantEnv {
			return
		}
	}
	t.Fatalf("tunnel environment is missing port-free %s", wantEnv)
}

func TestConfigMapInformer_UsesResourceNameFieldSelector(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	var listSelector string
	watchCalled := make(chan struct{}, 1)
	clientset.PrependReactor("list", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		listSelector = action.(k8stesting.ListAction).GetListRestrictions().Fields.String()
		return false, nil, nil
	})
	clientset.PrependWatchReactor("configmaps", func(action k8stesting.Action) (bool, watch.Interface, error) {
		if got := action.(k8stesting.WatchAction).GetWatchRestrictions().Fields.String(); got != listSelector {
			t.Errorf("watch selector %q differs from list selector %q", got, listSelector)
		}
		watchCalled <- struct{}{}
		return false, nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	factory := newConfigMapInformerFactory(clientset, "kubevpn")
	informer := factory.Core().V1().ConfigMaps().Informer()
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		t.Fatal("configmap informer did not sync")
	}
	select {
	case <-watchCalled:
	case <-time.After(time.Second):
		t.Fatal("configmap informer did not start a watch")
	}
	want := "metadata.name=" + config.ConfigMapPodTrafficManager
	if listSelector != want {
		t.Fatalf("list selector = %q, want %q", listSelector, want)
	}
}

func TestAttachmentSignature_ChangesWithConfig(t *testing.T) {
	ctx := context.Background()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data:       map[string]string{config.KeyNodeAttachments: "one"},
	}
	clientset := fake.NewSimpleClientset(cm)
	opts := Options{Clientset: clientset, ManagerNamespace: "kubevpn"}
	one, err := attachmentSignature(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := attachmentSignature(ctx, opts)
	if one != again {
		t.Fatal("same config should have same signature")
	}
	cm.Data[config.KeyNodeAttachments] = "two"
	_, _ = clientset.CoreV1().ConfigMaps("kubevpn").Update(ctx, cm, metav1.UpdateOptions{})
	two, _ := attachmentSignature(ctx, opts)
	if one == two {
		t.Fatal("changed config should have different signature")
	}
}
