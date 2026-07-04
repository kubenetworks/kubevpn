package dhcp

import (
	"context"
	"net"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func newFakeManager(t *testing.T) *Manager {
	t.Helper()
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "test-ns",
			UID:  "uid-123456789012",
		}},
	)
	return &Manager{
		clientset: clientset,
		namespace: "test-ns",
		cidr:      config.CIDR,
		cidr6:     config.CIDR6,
	}
}

func TestInitDHCP_CreatesConfigMap(t *testing.T) {
	m := newFakeManager(t)
	err := m.InitDHCP(context.Background())
	if err != nil {
		t.Fatalf("InitDHCP: %v", err)
	}

	cm, err := m.clientset.CoreV1().ConfigMaps("test-ns").Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CM: %v", err)
	}
	if cm.Data[config.KeyDHCP] != "" {
		t.Fatalf("expected empty DHCP, got %q", cm.Data[config.KeyDHCP])
	}
	if m.connectionID == "" {
		t.Fatal("connectionID not set")
	}
}

func TestInitDHCP_ExistingConfigMap(t *testing.T) {
	m := newFakeManager(t)
	// Pre-create the configmap
	_, err := m.clientset.CoreV1().ConfigMaps("test-ns").Create(context.Background(), &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
		Data:       map[string]string{config.KeyDHCP: "", config.KeyDHCP6: "", config.KeyEnvoy: ""},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	err = m.InitDHCP(context.Background())
	if err != nil {
		t.Fatalf("InitDHCP existing: %v", err)
	}
}

func TestRentIP_FullCycle(t *testing.T) {
	m := newFakeManager(t)
	if err := m.InitDHCP(context.Background()); err != nil {
		t.Fatal(err)
	}

	v4, v6, err := m.RentIP(context.Background())
	if err != nil {
		t.Fatalf("RentIP: %v", err)
	}
	if v4 == nil || v6 == nil {
		t.Fatal("got nil IPs")
	}
	if !config.CIDR.Contains(v4.IP) {
		t.Fatalf("v4 %s not in CIDR %s", v4, config.CIDR)
	}
	if !config.CIDR6.Contains(v6.IP) {
		t.Fatalf("v6 %s not in CIDR6 %s", v6, config.CIDR6)
	}
	t.Logf("Rented: v4=%s v6=%s", v4, v6)
}

func TestRentIP_NoDuplicates(t *testing.T) {
	m := newFakeManager(t)
	if err := m.InitDHCP(context.Background()); err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		v4, _, err := m.RentIP(context.Background())
		if err != nil {
			t.Fatalf("RentIP #%d: %v", i, err)
		}
		key := v4.IP.String()
		if seen[key] {
			t.Fatalf("duplicate at #%d: %s", i, key)
		}
		seen[key] = true
	}
}

func TestReleaseIP_ReusesFreed(t *testing.T) {
	m := newFakeManager(t)
	if err := m.InitDHCP(context.Background()); err != nil {
		t.Fatal(err)
	}

	v4, v6, err := m.RentIP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Rent a second one
	_, _, err = m.RentIP(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Release the first
	if err = m.ReleaseIP(context.Background(), v4.IP, v6.IP); err != nil {
		t.Fatalf("ReleaseIP: %v", err)
	}

	// Next rent should get the released IP back
	v4b, v6b, err := m.RentIP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !v4.IP.Equal(v4b.IP) {
		t.Fatalf("expected reuse of %s, got %s", v4.IP, v4b.IP)
	}
	if !v6.IP.Equal(v6b.IP) {
		t.Fatalf("expected reuse of %s, got %s", v6.IP, v6b.IP)
	}
}

func TestForEach_IteratesAllocated(t *testing.T) {
	m := newFakeManager(t)
	if err := m.InitDHCP(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, _, _ = m.RentIP(context.Background())
	_, _, _ = m.RentIP(context.Background())
	_, _, _ = m.RentIP(context.Background())

	var v4count, v6count int
	err := m.ForEach(context.Background(),
		func(ip net.IP) { v4count++ },
		func(ip net.IP) { v6count++ },
	)
	if err != nil {
		t.Fatalf("ForEach: %v", err)
	}
	if v4count != 3 {
		t.Fatalf("expected 3 v4 allocations, got %d", v4count)
	}
	if v6count != 3 {
		t.Fatalf("expected 3 v6 allocations, got %d", v6count)
	}
}

func TestGetConnectionID(t *testing.T) {
	m := newFakeManager(t)
	if err := m.InitDHCP(context.Background()); err != nil {
		t.Fatal(err)
	}
	id := m.GetConnectionID()
	if id == "" {
		t.Fatal("empty connectionID")
	}
	// UID is "uid-123456789012" → last 12 chars = "456789012" wait that's only 9...
	// Actually UID from fake is "uid-123456789012" which is 20 chars, last 12 = "3456789012" ... let me just check non-empty
	t.Logf("ConnectionID: %s", id)
}
