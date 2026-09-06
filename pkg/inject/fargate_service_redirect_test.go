package inject

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

// TestFargate_Inject_AlreadyInjected_StillRedirectsService is an integration test for the
// fargate/service-mode regression: when the backing workload already carries kubevpn
// sidecars for the current manager, fargateInjector.Inject used to return early and skip
// ModifyServiceTargetPort, so a `proxy svc/... --portmap` never redirected the Service to
// the envoy listener — matched traffic fell through to the real workload (local 9080)
// instead of the port-mapped local port (8080).
//
// It wires the real components end to end against a fake clientset: the injector writes the
// envoy rule into the traffic-manager ConfigMap (VirtualStore) AND updates the Service's
// targetPorts, and we assert the Service now targets the exact envoy listener port the rule
// advertises, with the original target ports saved for restore.
func TestFargate_Inject_AlreadyInjected_StillRedirectsService(t *testing.T) {
	const (
		managerNS  = "kubevpn"
		workloadNS = "default"
		nodeID     = "services.reviews"
		svcName    = "reviews"
	)

	// Backing deployment whose pod template is ALREADY fargate-injected for this manager
	// (so injectedForManager returns true and the old early-return path is taken).
	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: workloadNS},
		Spec: appsv1.DeploymentSpec{
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:  "reviews",
						Ports: []v1.ContainerPort{{ContainerPort: 9080, Protocol: v1.ProtocolTCP}},
					}},
				},
			},
		},
	}
	AddEnvoyAndSSHContainer(&dep.Spec.Template, workloadNS, nodeID, false, managerNS, "img:test")
	if !injectedForManager(&dep.Spec.Template, managerNS) {
		t.Fatal("precondition: expected the deployment to be injected-for-manager")
	}

	unstructedMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(dep)
	if err != nil {
		t.Fatalf("to unstructured: %v", err)
	}

	// Service targeting the original app port, plus the traffic-manager ConfigMap the
	// VirtualStore reads/writes.
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: workloadNS},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{{Name: "http", Port: 9080, TargetPort: intstr.FromInt32(9080)}},
		},
	}
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: managerNS},
		Data:       map[string]string{},
	}
	clientset := fake.NewSimpleClientset(svc, cm)

	injector := &fargateInjector{opts: InjectOptions{
		Clientset:        clientset,
		ManagerNamespace: managerNS,
		NodeID:           nodeID,
		OwnerID:          "owner-1",
		Headers:          map[string]string{"env": "test"},
		PortMaps:         []string{"9080:8080"},
		Object:           &resource.Info{Name: svcName},
		Controller: &resource.Info{
			Name:      svcName,
			Namespace: workloadNS,
			Object:    &unstructured.Unstructured{Object: unstructedMap},
			Mapping: &meta.RESTMapping{
				Resource: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			},
		},
	}}

	if err = injector.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	// The envoy rule must have been written with a fargate listener port + the 8080 portmap.
	got, err := clientset.CoreV1().ConfigMaps(managerNS).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var virtuals []*controlplane.Virtual
	if err = yaml.Unmarshal([]byte(got.Data[config.KeyEnvoy]), &virtuals); err != nil {
		t.Fatalf("decode envoy config: %v", err)
	}
	var listenerPort int32
	var found bool
	for _, virtual := range virtuals {
		if virtual.UID != nodeID || virtual.Namespace != workloadNS {
			continue
		}
		found = true
		if !virtual.FargateMode {
			t.Error("expected fargate-mode virtual")
		}
		for _, p := range virtual.Ports {
			if p.ContainerPort == 9080 {
				listenerPort = p.EnvoyListenerPort
			}
		}
		if len(virtual.Rules) != 1 || virtual.Rules[0].PortMap[9080] == "" {
			t.Fatalf("expected one rule with a portmap for 9080, got %+v", virtual.Rules)
		}
		// portmap value is "<envoyRulePort>:<localPort>" — localPort must be the mapped 8080.
		if pm := virtual.Rules[0].ParsePortMap(); len(pm) == 0 || pm[0].LocalPort != 8080 {
			t.Fatalf("expected local port 8080 in portmap, got %+v", virtual.Rules[0].PortMap)
		}
	}
	if !found {
		t.Fatalf("envoy rule for %s/%s not written", workloadNS, nodeID)
	}
	if listenerPort == 0 {
		t.Fatal("envoy listener port for containerPort 9080 was not assigned")
	}

	// The Service must now target that exact envoy listener port (the fix), with the
	// original target port saved for restore.
	updatedSvc, err := clientset.CoreV1().Services(workloadNS).Get(context.Background(), svcName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updatedSvc.Spec.Ports[0].TargetPort.IntVal != listenerPort {
		t.Fatalf("service targetPort: want envoy listener %d, got %d (service redirect was skipped)",
			listenerPort, updatedSvc.Spec.Ports[0].TargetPort.IntVal)
	}
	if _, ok := updatedSvc.Annotations[annotationOriginalTargetPorts]; !ok {
		t.Errorf("expected %q annotation saving the original target ports", annotationOriginalTargetPorts)
	}
}
