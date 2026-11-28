//go:build integration

package handler

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

type nodeBoundaryWorkloadIdentity struct {
	generation int64
	podUID     types.UID
}

func captureWorkloadIdentity(t *testing.T, clientset kubernetes.Interface, namespace, deployment, app string) nodeBoundaryWorkloadIdentity {
	t.Helper()
	deploy, err := clientset.AppsV1().Deployments(namespace).Get(context.Background(), deployment, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pods, err := clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app=" + app})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("list pods for %s: %v", app, err)
	}
	return nodeBoundaryWorkloadIdentity{generation: deploy.Generation, podUID: pods.Items[0].UID}
}

func assertWorkloadIdentityUnchanged(t *testing.T, clientset kubernetes.Interface, namespace, deployment, app string, before nodeBoundaryWorkloadIdentity) {
	t.Helper()
	after := captureWorkloadIdentity(t, clientset, namespace, deployment, app)
	if after != before {
		t.Fatalf("attachment changed workload identity: before=%+v after=%+v", before, after)
	}
	pods, err := clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app=" + app})
	if err != nil {
		t.Fatal(err)
	}
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			if container.Name == config.ContainerSidecarVPN || container.Name == config.ContainerSidecarEnvoy {
				t.Fatalf("traffic sidecar %s found in %s/%s", container.Name, pod.Namespace, pod.Name)
			}
		}
	}
}

func captureServiceTargetPorts(t *testing.T, clientset kubernetes.Interface, namespace, name string) map[string]intstr.IntOrString {
	t.Helper()
	service, err := clientset.CoreV1().Services(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ports := make(map[string]intstr.IntOrString, len(service.Spec.Ports))
	for _, port := range service.Spec.Ports {
		ports[fmt.Sprintf("%d/%s", port.Port, port.Protocol)] = port.TargetPort
	}
	return ports
}

func assertServiceTargetPortsUnchanged(t *testing.T, clientset kubernetes.Interface, namespace, name string, before map[string]intstr.IntOrString) {
	t.Helper()
	after := captureServiceTargetPorts(t, clientset, namespace, name)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("Service targetPort changed: before=%v after=%v", before, after)
	}
}
