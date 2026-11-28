package xds

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

const publisherVirtualsYAML = `- schemaVersion: 2
  Namespace: default
  Uid: deployments.apps.reviews
  selector: app=reviews
  Ports:
  - containerPort: 9080
    protocol: TCP
  - containerPort: 9081
    protocol: UDP
  Rules:
  - ownerID: owner-1
    LocalTunIPv4: 198.18.0.1
    isAgent: true
`

func newPublisherTestPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "reviews-x", Namespace: "default", UID: "uid-1", Labels: map[string]string{"app": "reviews"}},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.0.5"},
	}
}

func TestAttachmentPublisher_PublishesAndRemoves(t *testing.T) {
	ctx := context.Background()
	pod := newPublisherTestPod()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data:       map[string]string{config.KeyEnvoy: publisherVirtualsYAML},
	}
	desiredBefore := cm.Data[config.KeyEnvoy]
	clientset := fake.NewSimpleClientset(cm, pod)
	p := NewAttachmentPublisher(clientset, "kubevpn")
	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Data[config.KeyEnvoy] != desiredBefore {
		t.Fatal("publisher mutated desired Virtuals")
	}
	var attachments []controlplane.NodeAttachment
	if err := yaml.Unmarshal([]byte(got.Data[config.KeyNodeAttachments]), &attachments); err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 1 || attachments[0].PodUID != "uid-1" || attachments[0].NodeName != "node-a" || attachments[0].PodIP != "10.244.0.5" {
		t.Fatalf("unexpected attachments: %+v", attachments)
	}
	if attachments[0].AgentListenerPort == 0 {
		t.Fatal("admin base port not allocated")
	}
	if attachments[0].NodeID == "" || len(attachments[0].Ports) != 2 {
		t.Fatalf("derived node or port mappings missing: %+v", attachments[0])
	}
	seenPorts := map[int32]bool{}
	for _, port := range attachments[0].Ports {
		if port.AgentListenerPort == 0 || seenPorts[port.AgentListenerPort] {
			t.Fatalf("invalid per-pod listener allocation: %+v", attachments[0].Ports)
		}
		seenPorts[port.AgentListenerPort] = true
	}
	var derived []*controlplane.Virtual
	if err := yaml.Unmarshal([]byte(got.Data[config.KeyAgentXDS]), &derived); err != nil {
		t.Fatal(err)
	}
	if len(derived) != 1 || derived[0].UID != attachments[0].NodeID || len(derived[0].AgentOrigins) != 1 {
		t.Fatalf("unexpected derived state: %+v", derived)
	}
	if derived[0].AgentListenerPort != attachments[0].AgentListenerPort {
		t.Fatal("derived admin port differs from attachment port")
	}
	if derived[0].AgentOrigins[0].PodIP != pod.Status.PodIP || len(derived[0].AgentOrigins[0].Ports) != 2 {
		t.Fatalf("derived origin does not identify the real pod: %+v", derived[0].AgentOrigins[0])
	}

	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	gotAgain, _ := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if gotAgain.Data[config.KeyNodeAttachments] != got.Data[config.KeyNodeAttachments] || gotAgain.Data[config.KeyAgentXDS] != got.Data[config.KeyAgentXDS] {
		t.Fatal("attachments or derived virtuals changed on an identical reconcile")
	}
	if gotAgain.Data[config.KeyEnvoy] != desiredBefore {
		t.Fatal("identical reconcile changed desired state")
	}
	allocationState := gotAgain.Data[config.KeyAgentPortAllocations]
	if allocationState == "" {
		t.Fatal("listener allocation state was not persisted")
	}

	_ = clientset.CoreV1().Pods("default").Delete(ctx, pod.Name, metav1.DeleteOptions{})
	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	attachments = nil
	_ = yaml.Unmarshal([]byte(got.Data[config.KeyNodeAttachments]), &attachments)
	if len(attachments) != 0 {
		t.Fatalf("stale attachments: %+v", attachments)
	}
	derived = nil
	_ = yaml.Unmarshal([]byte(got.Data[config.KeyAgentXDS]), &derived)
	if len(derived) != 0 {
		t.Fatalf("stale derived virtuals: %+v", derived)
	}
	if got.Data[config.KeyEnvoy] != desiredBefore {
		t.Fatal("pod removal changed desired state")
	}
	if got.Data[config.KeyAgentPortAllocations] != allocationState {
		t.Fatal("listener allocation state was discarded with the attachment")
	}
}

func TestAttachmentPublisher_PublishesServiceOriginsPerNode(t *testing.T) {
	ctx := context.Background()
	podA := newPublisherTestPod()
	podB := newPublisherTestPod()
	podB.Name = "reviews-y"
	podB.UID = "uid-2"
	podB.Spec.NodeName = "node-b"
	podB.Status.PodIP = "10.244.0.6"
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "reviews", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector:  map[string]string{"app": "reviews"},
			ClusterIP: "10.96.0.10",
			Ports: []corev1.ServicePort{{
				Protocol: corev1.ProtocolTCP, Port: 80, TargetPort: intstr.FromInt(9080),
			}},
		},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data:       map[string]string{config.KeyEnvoy: publisherVirtualsYAML},
	}
	clientset := fake.NewSimpleClientset(cm, podA, podB, service)
	p := NewAttachmentPublisher(clientset, "kubevpn")
	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var attachments []controlplane.NodeAttachment
	if err := yaml.Unmarshal([]byte(got.Data[config.KeyNodeAttachments]), &attachments); err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 4 {
		t.Fatalf("expected two pod and two per-node service attachments: %+v", attachments)
	}
	serviceAttachments := 0
	for _, attachment := range attachments {
		if attachment.ServiceName == "" {
			continue
		}
		serviceAttachments++
		if attachment.PodIP != "10.96.0.10" || attachment.OriginIP != "10.96.0.10" {
			t.Fatalf("service destination/origin mismatch: %+v", attachment)
		}
		if len(attachment.Ports) != 1 || attachment.Ports[0].ContainerPort != 9080 || attachment.Ports[0].OriginPort != 80 {
			t.Fatalf("service port was not mapped to the workload port: %+v", attachment.Ports)
		}
	}
	if serviceAttachments != 2 {
		t.Fatalf("expected one service attachment per node, got %d", serviceAttachments)
	}
	var virtuals []*controlplane.Virtual
	if err := yaml.Unmarshal([]byte(got.Data[config.KeyAgentXDS]), &virtuals); err != nil {
		t.Fatal(err)
	}
	if len(virtuals) != 2 {
		t.Fatalf("expected two node virtuals: %+v", virtuals)
	}
	serviceOrigins := 0
	for _, derived := range virtuals {
		for _, origin := range derived.AgentOrigins {
			if origin.ServiceName != "" {
				serviceOrigins++
				if origin.OriginIP != "10.96.0.10" || origin.PodIP != "10.96.0.10" {
					t.Fatalf("derived service origin lost ClusterIP: %+v", origin)
				}
			}
		}
	}
	if serviceOrigins != 2 {
		t.Fatalf("expected derived service origin on both nodes, got %d", serviceOrigins)
	}
}

func TestAttachmentPublisher_ExplicitServiceUsesOnlyRequestedClusterIP(t *testing.T) {
	base := &controlplane.Virtual{
		SchemaVersion: controlplane.CurrentSchemaVersion, Namespace: "default", UID: "services.reviews", ServiceName: "reviews", Selector: "app=reviews",
		Ports: []controlplane.ContainerPort{{ContainerPort: 9080, Protocol: corev1.ProtocolTCP}},
		Rules: []*controlplane.Rule{{OwnerID: "owner-a", LocalTunIPv4: "198.18.0.1", IsAgent: true}},
	}
	data, err := yaml.Marshal([]*controlplane.Virtual{base})
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"}, Data: map[string]string{config.KeyEnvoy: string(data)}}
	reviews := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "reviews", Namespace: "default"}, Spec: corev1.ServiceSpec{
		Selector: map[string]string{"app": "reviews"}, ClusterIP: "10.96.0.10",
		Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt(9080), Protocol: corev1.ProtocolTCP}},
	}}
	other := reviews.DeepCopy()
	other.Name = "reviews-other"
	other.Spec.ClusterIP = "10.96.0.20"
	clientset := fake.NewSimpleClientset(cm, newPublisherTestPod(), reviews, other)
	p := NewAttachmentPublisher(clientset, "kubevpn")
	if err := p.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var attachments []controlplane.NodeAttachment
	if err := yaml.Unmarshal([]byte(got.Data[config.KeyNodeAttachments]), &attachments); err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 1 || attachments[0].ServiceName != "reviews" || attachments[0].PodIP != "10.96.0.10" || attachments[0].OriginIP != "10.96.0.10" {
		t.Fatalf("explicit Service must produce only its own ClusterIP attachment: %+v", attachments)
	}
	if len(attachments[0].Ports) != 1 || attachments[0].Ports[0].OriginPort != 80 {
		t.Fatalf("service port mapping missing: %+v", attachments[0].Ports)
	}
}

func TestAttachmentPublisher_ExplicitServiceSuppressesInferredDuplicate(t *testing.T) {
	explicit := &controlplane.Virtual{Namespace: "default", UID: "services.reviews", ServiceName: "reviews", Selector: "app=reviews", Ports: []controlplane.ContainerPort{{ContainerPort: 9080, Protocol: corev1.ProtocolTCP}}, Rules: []*controlplane.Rule{{OwnerID: "owner-a", LocalTunIPv4: "198.18.0.1", IsAgent: true}}}
	backend := &controlplane.Virtual{Namespace: "default", UID: "deployments.apps.reviews", Selector: "app=reviews", Ports: []controlplane.ContainerPort{{ContainerPort: 9080, Protocol: corev1.ProtocolTCP}}, Rules: []*controlplane.Rule{{OwnerID: "owner-b", LocalTunIPv4: "198.18.0.2", IsAgent: true}}}
	data, err := yaml.Marshal([]*controlplane.Virtual{explicit, backend})
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"}, Data: map[string]string{config.KeyEnvoy: string(data)}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "reviews", Namespace: "default"}, Spec: corev1.ServiceSpec{
		Selector: map[string]string{"app": "reviews"}, ClusterIP: "10.96.0.10",
		Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt(9080), Protocol: corev1.ProtocolTCP}},
	}}
	clientset := fake.NewSimpleClientset(cm, newPublisherTestPod(), service)
	if err := NewAttachmentPublisher(clientset, "kubevpn").reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var attachments []controlplane.NodeAttachment
	if err := yaml.Unmarshal([]byte(got.Data[config.KeyNodeAttachments]), &attachments); err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 2 {
		t.Fatalf("expected one explicit Service and one backend pod, got %+v", attachments)
	}
	for _, attachment := range attachments {
		if attachment.ServiceName == "reviews" && attachment.WorkloadUID != "services.reviews" {
			t.Fatalf("inferred duplicate Service attachment was not suppressed: %+v", attachment)
		}
	}
}

func TestAttachmentPublisher_SkipsSidecarPod(t *testing.T) {
	ctx := context.Background()
	pod := newPublisherTestPod()
	pod.Spec.Containers = []corev1.Container{{Name: config.ContainerSidecarVPN}, {Name: config.ContainerSidecarEnvoy}}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data:       map[string]string{config.KeyEnvoy: publisherVirtualsYAML},
	}
	clientset := fake.NewSimpleClientset(cm, pod)
	p := NewAttachmentPublisher(clientset, "kubevpn")
	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	var attachments []controlplane.NodeAttachment
	_ = yaml.Unmarshal([]byte(got.Data[config.KeyNodeAttachments]), &attachments)
	if len(attachments) != 0 {
		t.Fatalf("sidecar pod must not get attachment: %+v", attachments)
	}
}

func TestAttachmentPublisher_RejectsConflictingUDPRoutes(t *testing.T) {
	virtual := &controlplane.Virtual{
		SchemaVersion: controlplane.CurrentSchemaVersion, Namespace: "default", UID: "deployments.apps.reviews", Selector: "app=reviews",
		Ports: []controlplane.ContainerPort{{ContainerPort: 9081, Protocol: corev1.ProtocolUDP}},
		Rules: []*controlplane.Rule{
			{OwnerID: "owner-a", LocalTunIPv4: "198.18.0.1", IsAgent: true},
			{OwnerID: "owner-b", LocalTunIPv4: "198.18.0.2", IsAgent: true},
		},
	}
	data, err := yaml.Marshal([]*controlplane.Virtual{virtual})
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"}, Data: map[string]string{config.KeyEnvoy: string(data)}}
	p := NewAttachmentPublisher(fake.NewSimpleClientset(cm, newPublisherTestPod()), "kubevpn")
	if err := p.reconcileOnce(context.Background()); err == nil {
		t.Fatal("expected conflicting UDP routes to be rejected")
	}
}

func TestAttachmentPublisher_ResolvesSyncTargetOnlyInDerivedVirtual(t *testing.T) {
	ctx := context.Background()
	base := &controlplane.Virtual{
		SchemaVersion: controlplane.CurrentSchemaVersion,
		Namespace:     "default",
		UID:           "deployments.apps.reviews",
		Selector:      "app=reviews",
		Ports:         []controlplane.ContainerPort{{ContainerPort: 9080, Protocol: corev1.ProtocolTCP}},
		Rules: []*controlplane.Rule{{
			OwnerID: "owner-sync", IsAgent: true, Headers: map[string]string{"env": "test"},
			PortMap:    map[int32]string{9080: "9080"},
			SyncTarget: &controlplane.SyncTarget{UID: "deployments.apps.reviews-sync-a", Selector: "owner-ref=reviews-sync-a"},
		}},
	}
	data, err := yaml.Marshal([]*controlplane.Virtual{base})
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"}, Data: map[string]string{config.KeyEnvoy: string(data)}}
	now := time.Now()
	oldClone := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-old", Namespace: "default", UID: "clone-old", Labels: map[string]string{"owner-ref": "reviews-sync-a"}, CreationTimestamp: metav1.NewTime(now.Add(-time.Minute))},
		Spec:       corev1.PodSpec{NodeName: "node-b"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.1.10", Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}},
	}
	clientset := fake.NewSimpleClientset(cm, newPublisherTestPod(), oldClone)
	p := NewAttachmentPublisher(clientset, "kubevpn")
	readVirtuals := func(key string) []*controlplane.Virtual {
		t.Helper()
		got, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var virtuals []*controlplane.Virtual
		if err := yaml.Unmarshal([]byte(got.Data[key]), &virtuals); err != nil {
			t.Fatal(err)
		}
		return virtuals
	}
	assertEndpoints := func(wantBase, wantDerived string) {
		t.Helper()
		baseVirtuals := readVirtuals(config.KeyEnvoy)
		derivedVirtuals := readVirtuals(config.KeyAgentXDS)
		if len(baseVirtuals) != 1 || len(derivedVirtuals) != 1 {
			t.Fatalf("expected one desired and one derived virtual: base=%+v derived=%+v", baseVirtuals, derivedVirtuals)
		}
		if got := baseVirtuals[0].Rules[0].LocalTunIPv4; got != wantBase {
			t.Fatalf("base sync endpoint = %q, want %q", got, wantBase)
		}
		if got := derivedVirtuals[0].Rules[0].LocalTunIPv4; got != wantDerived {
			t.Fatalf("derived sync endpoint = %q, want %q", got, wantDerived)
		}
	}

	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertEndpoints("", oldClone.Status.PodIP)

	newClone := oldClone.DeepCopy()
	newClone.Name = "clone-new"
	newClone.UID = "clone-new"
	newClone.ResourceVersion = ""
	newClone.CreationTimestamp = metav1.NewTime(now)
	newClone.Status.PodIP = "10.244.1.11"
	newClone.Status.Conditions[0].Status = corev1.ConditionFalse
	if _, err := clientset.CoreV1().Pods("default").Create(ctx, newClone, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertEndpoints("", oldClone.Status.PodIP)

	newClone.Status.Conditions[0].Status = corev1.ConditionTrue
	if _, err := clientset.CoreV1().Pods("default").Update(ctx, newClone, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := clientset.CoreV1().Pods("default").Delete(ctx, oldClone.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertEndpoints("", newClone.Status.PodIP)

	if err := clientset.CoreV1().Pods("default").Delete(ctx, newClone.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertEndpoints("", "")
}

func TestAttachmentPublisher_SyncTargetListFailurePreservesLastGoodState(t *testing.T) {
	ctx := context.Background()
	base := &controlplane.Virtual{
		SchemaVersion: controlplane.CurrentSchemaVersion, Namespace: "default", UID: "deployments.apps.reviews", Selector: "app=reviews",
		Ports: []controlplane.ContainerPort{{ContainerPort: 9080, Protocol: corev1.ProtocolTCP}},
		Rules: []*controlplane.Rule{{OwnerID: "owner-sync", IsAgent: true, SyncTarget: &controlplane.SyncTarget{UID: "deployments.apps.reviews-sync-a", Selector: "owner-ref=reviews-sync-a"}}},
	}
	data, err := yaml.Marshal([]*controlplane.Virtual{base})
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"}, Data: map[string]string{config.KeyEnvoy: string(data)}}
	clone := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "clone", Namespace: "default", UID: "clone", Labels: map[string]string{"owner-ref": "reviews-sync-a"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.1.10", Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}},
	}
	clientset := fake.NewSimpleClientset(cm, newPublisherTestPod(), clone)
	p := NewAttachmentPublisher(clientset, "kubevpn")
	if err := p.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	clientset.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		selector := action.(k8stesting.ListAction).GetListRestrictions().Labels.String()
		if selector == "owner-ref=reviews-sync-a" {
			return true, nil, errors.New("injected sync target list failure")
		}
		return false, nil, nil
	})
	if err := p.reconcileOnce(ctx); err == nil {
		t.Fatal("expected sync target list failure")
	}
	after, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{config.KeyEnvoy, config.KeyAgentXDS, config.KeyNodeAttachments, config.KeyAgentPortAllocations} {
		if after.Data[key] != before.Data[key] {
			t.Fatalf("failed reconcile changed %s", key)
		}
	}
}
