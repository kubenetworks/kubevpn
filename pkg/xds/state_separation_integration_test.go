package xds

import (
	"context"
	"sync/atomic"
	"testing"

	envoycache "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	logrus "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func TestIntegration_StateSeparation_PublisherFeedsXDSWithoutMutatingDesired(t *testing.T) {
	ctx := context.Background()
	desired := []*controlplane.Virtual{{
		SchemaVersion: controlplane.CurrentSchemaVersion,
		Namespace:     "default",
		UID:           "deployments.apps.reviews",
		Selector:      "app=reviews",
		Ports:         []controlplane.ContainerPort{{ContainerPort: 9080, Protocol: corev1.ProtocolTCP}},
		Rules: []*controlplane.Rule{
			{OwnerID: "owner-a", LocalTunIPv4: "198.18.0.1", IsAgent: true, Headers: map[string]string{"user": "a"}},
			{OwnerID: "owner-b", LocalTunIPv4: "198.18.0.2", IsAgent: true, Headers: map[string]string{"user": "b"}},
		},
	}}
	desiredYAML, err := yaml.Marshal(desired)
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data:       map[string]string{config.KeyEnvoy: string(desiredYAML)},
	}
	clientset := fake.NewSimpleClientset(cm, newPublisherTestPod())
	publisher := NewAttachmentPublisher(clientset, "kubevpn")
	if err := publisher.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Data[config.KeyEnvoy] != string(desiredYAML) {
		t.Fatal("publisher rewrote desired state")
	}
	var derived []*controlplane.Virtual
	if err := yaml.Unmarshal([]byte(stored.Data[config.KeyAgentXDS]), &derived); err != nil {
		t.Fatal(err)
	}
	if len(derived) != 1 || len(derived[0].Rules) != 2 {
		t.Fatalf("derived state did not preserve both owners: %+v", derived)
	}

	snapshotCache := envoycache.NewSnapshotCache(false, envoycache.IDHash{}, logrus.New())
	processor := newProcessor(snapshotCache, logrus.NewEntry(logrus.New()))
	if err := processor.ProcessFile(NotifyMessage{Content: stored.Data[config.KeyAgentXDS]}); err != nil {
		t.Fatal(err)
	}
	nodeID := util.GenEnvoyUID("default", "deployments.apps.reviews.nodes.node-a")
	snapshot, err := snapshotCache.GetSnapshot(nodeID)
	if err != nil {
		t.Fatalf("derived state did not reach xDS: %v", err)
	}
	if len(snapshot.GetResources(resource.ListenerType)) == 0 {
		t.Fatal("derived xDS snapshot has no listeners")
	}
}

func TestAttachmentPublisher_IdenticalReconcileSkipsUpdate(t *testing.T) {
	ctx := context.Background()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data:       map[string]string{config.KeyEnvoy: publisherVirtualsYAML},
	}
	clientset := fake.NewSimpleClientset(cm, newPublisherTestPod())
	publisher := NewAttachmentPublisher(clientset, "kubevpn")
	if err := publisher.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Data[config.KeyEnvoy] != publisherVirtualsYAML || stored.Data[config.KeyAgentXDS] == "" {
		t.Fatal("publisher did not preserve desired state and publish separate derived state")
	}

	var updates int32
	clientset.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		atomic.AddInt32(&updates, 1)
		return false, nil, nil
	})
	if err := publisher.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&updates); got != 0 {
		t.Fatalf("identical reconcile performed %d updates", got)
	}
}

func TestAttachmentPublisher_RestartRebuildsSameDerivedState(t *testing.T) {
	ctx := context.Background()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data:       map[string]string{config.KeyEnvoy: publisherVirtualsYAML},
	}
	clientset := fake.NewSimpleClientset(cm, newPublisherTestPod())
	if err := NewAttachmentPublisher(clientset, "kubevpn").reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Data[config.KeyEnvoy] != publisherVirtualsYAML {
		t.Fatal("publisher changed desired state")
	}
	derivedBefore := stored.Data[config.KeyAgentXDS]
	allocationsBefore := stored.Data[config.KeyAgentPortAllocations]
	if derivedBefore == "" || allocationsBefore == "" {
		t.Fatal("initial derived state or allocations are missing")
	}
	stored.Data[config.KeyAgentXDS] = `- Namespace: stale
  Uid: stale.nodes.node-a
  agentOrigins:
  - podUID: stale
`
	if _, err := clientset.CoreV1().ConfigMaps("kubevpn").Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := NewAttachmentPublisher(clientset, "kubevpn").reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Data[config.KeyAgentXDS] != derivedBefore || rebuilt.Data[config.KeyAgentPortAllocations] != allocationsBefore {
		t.Fatal("publisher restart did not deterministically rebuild derived state")
	}
	if rebuilt.Data[config.KeyEnvoy] != publisherVirtualsYAML {
		t.Fatal("publisher restart changed desired state")
	}
}
