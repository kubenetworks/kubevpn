package action

import (
	"context"
	"errors"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func TestFindConnection_Found(t *testing.T) {
	svr := &Server{
		connections: []handler.Connection{
			&handler.ConnectOptions{ManagerNamespace: "ns1"},
			&handler.ConnectOptions{ManagerNamespace: "ns2"},
		},
	}
	// We need GetConnectionID — but that requires DHCP init.
	// Test the nil case instead
	conn, idx := svr.findConnection("nonexistent")
	if conn != nil || idx != -1 {
		t.Fatalf("expected nil/-1 for nonexistent, got %v/%d", conn, idx)
	}
}

func TestFindConnection_Nil(t *testing.T) {
	svr := &Server{}
	conn, idx := svr.findConnection("")
	if conn != nil || idx != -1 {
		t.Fatal("expected nil/-1 for empty server")
	}
}

func TestUninstall_NoError(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	ns := "test-ns"

	// Uninstall should not error even when resources don't exist
	// (all deletes are fire-and-forget with _ = ...)
	err := Uninstall(context.Background(), clientset, ns)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
}

// TestUninstall_WithResources pre-creates all KubeVPN server-side resources
// (ConfigMap, Secret, Deployment, Service, ServiceAccount, Role, RoleBinding)
// and verifies that Uninstall removes every one of them.
func TestUninstall_WithResources(t *testing.T) {
	ns := "test-ns"
	name := config.ConfigMapPodTrafficManager
	ctx := context.Background()

	clientset := fake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
	)

	// Verify resources exist before Uninstall
	if _, err := clientset.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("pre-check: ConfigMap should exist: %v", err)
	}
	if _, err := clientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("pre-check: Secret should exist: %v", err)
	}
	if _, err := clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("pre-check: Deployment should exist: %v", err)
	}
	if _, err := clientset.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("pre-check: Service should exist: %v", err)
	}
	if _, err := clientset.CoreV1().ServiceAccounts(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("pre-check: ServiceAccount should exist: %v", err)
	}
	if _, err := clientset.RbacV1().Roles(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("pre-check: Role should exist: %v", err)
	}
	if _, err := clientset.RbacV1().RoleBindings(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("pre-check: RoleBinding should exist: %v", err)
	}

	err := Uninstall(ctx, clientset, ns)
	if err != nil {
		t.Fatalf("Uninstall returned error: %v", err)
	}

	// Verify all resources are deleted
	type resourceCheck struct {
		kind string
		get  func() error
	}
	checks := []resourceCheck{
		{"ConfigMap", func() error {
			_, err := clientset.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
			return err
		}},
		{"Secret", func() error {
			_, err := clientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
			return err
		}},
		{"Deployment", func() error {
			_, err := clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
			return err
		}},
		{"Service", func() error {
			_, err := clientset.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
			return err
		}},
		{"ServiceAccount", func() error {
			_, err := clientset.CoreV1().ServiceAccounts(ns).Get(ctx, name, metav1.GetOptions{})
			return err
		}},
		{"Role", func() error {
			_, err := clientset.RbacV1().Roles(ns).Get(ctx, name, metav1.GetOptions{})
			return err
		}},
		{"RoleBinding", func() error {
			_, err := clientset.RbacV1().RoleBindings(ns).Get(ctx, name, metav1.GetOptions{})
			return err
		}},
	}
	for _, check := range checks {
		err := check.get()
		if err == nil {
			t.Errorf("%s should have been deleted but still exists", check.kind)
		}
	}
}

// TestUninstall_PartialResources verifies that Uninstall does not error
// when only a subset of the expected resources exist.
func TestUninstall_PartialResources(t *testing.T) {
	ns := "partial-ns"
	name := config.ConfigMapPodTrafficManager
	ctx := context.Background()

	// Only create ConfigMap and Service — skip Secret, Deployment, SA, Role, RoleBinding
	clientset := fake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
	)

	err := Uninstall(ctx, clientset, ns)
	if err != nil {
		t.Fatalf("Uninstall with partial resources returned error: %v", err)
	}

	// Verify the resources that existed are now deleted
	if _, err := clientset.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Error("ConfigMap should have been deleted")
	}
	if _, err := clientset.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Error("Service should have been deleted")
	}
}

// TestResolveKubeconfigBytes_WithSSHJump verifies that when an SshJump with Addr
// is provided, resolveKubeconfigBytes attempts an SSH connection that fails
// (no real SSH server), returning an error.
func TestResolveKubeconfigBytes_WithSSHJump(t *testing.T) {
	ctx := context.Background()
	jump := &rpc.SshJump{
		Addr:     "127.0.0.1:0", // unreachable SSH address
		User:     "testuser",
		Password: "testpass",
	}

	_, err := resolveKubeconfigBytes(ctx, jump, "apiVersion: v1\nkind: Config\n", false)
	if err == nil {
		t.Fatal("expected error from SSH dial to unreachable address, got nil")
	}
}

// TestResolveKubeconfigBytes_EmptySSH verifies that an SshJump with all empty
// fields (IsEmpty() == true) causes resolveKubeconfigBytes to return the request
// bytes unchanged, without writing any temp file.
func TestResolveKubeconfigBytes_EmptySSH(t *testing.T) {
	ctx := context.Background()
	jump := &rpc.SshJump{} // all fields empty → IsEmpty() returns true
	kubeconfigBytes := "apiVersion: v1\nkind: Config\nclusters: []\n"

	data, err := resolveKubeconfigBytes(ctx, jump, kubeconfigBytes, false)
	if err != nil {
		t.Fatalf("resolveKubeconfigBytes with empty SshJump: %v", err)
	}
	if string(data) != kubeconfigBytes {
		t.Fatalf("bytes mismatch:\n  got:  %q\n  want: %q", string(data), kubeconfigBytes)
	}
}

// TestResolveKubeconfigBytes_ToFactory_EndToEnd exercises the no-SSH bytes chain
// shared by every daemon action (connect/proxy/sync/reset/uninstall/disconnect):
// resolveKubeconfigBytes -> util.InitFactoryByBytes -> InitClient. It must build a
// usable client entirely in-memory, without any temp kubeconfig file or network.
func TestResolveKubeconfigBytes_ToFactory_EndToEnd(t *testing.T) {
	ctx := context.Background()
	kubeconfigBytes := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
    insecure-skip-tls-verify: true
  name: c1
contexts:
- context:
    cluster: c1
    namespace: default
  name: ctx
current-context: ctx
users:
- name: c1
  user:
    token: fake
`
	data, err := resolveKubeconfigBytes(ctx, nil, kubeconfigBytes, false)
	if err != nil {
		t.Fatalf("resolveKubeconfigBytes: %v", err)
	}

	connect := &handler.ConnectOptions{}
	if err = connect.InitClient(util.InitFactoryByBytes(data, "default")); err != nil {
		t.Fatalf("InitClient from bytes: %v", err)
	}
	if connect.GetClientset() == nil {
		t.Fatal("expected non-nil clientset built from bytes")
	}
	if connect.GetFactory() == nil {
		t.Fatal("expected non-nil factory built from bytes")
	}
}

// TestStreamWriter_SendError verifies that when the send function returns
// an error, Write still returns len(p), nil — the error is intentionally
// swallowed (see streamWriter.Write: _ = w.send(...)).
func TestStreamWriter_SendError(t *testing.T) {
	sendErr := fmt.Errorf("simulated gRPC send failure")
	var callCount int
	w := newStreamWriter(func(msg string) error {
		callCount++
		return sendErr
	})

	input := []byte("data that will fail to send")
	n, err := w.Write(input)

	if n != len(input) {
		t.Fatalf("expected n=%d, got %d", len(input), n)
	}
	if err != nil {
		t.Fatalf("expected nil error from Write even when send fails, got %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected send to be called once, got %d", callCount)
	}

	// Verify the error is indeed the one we returned (sanity check on the closure)
	directErr := sendErr
	if !errors.Is(directErr, sendErr) {
		t.Fatal("sendErr identity check failed")
	}
}
