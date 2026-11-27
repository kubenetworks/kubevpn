package manager

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TestGenRole_Fields verifies that genRole produces the expected ObjectMeta,
// and the single PolicyRule with the correct verbs, APIGroups, resources and
// resource-name filter.
func TestGenRole_Fields(t *testing.T) {
	const ns = "test-ns"
	role := genRole(ns)

	if role.Name != config.ConfigMapPodTrafficManager {
		t.Errorf("Name: got %q, want %q", role.Name, config.ConfigMapPodTrafficManager)
	}
	if role.Namespace != ns {
		t.Errorf("Namespace: got %q, want %q", role.Namespace, ns)
	}
	// Rule 0: configmaps/secrets (resourceName-scoped); Rules 1-2: CIDR detection
	// (pods/services, see genRole / docs/46); Rule 3: batch/jobs.
	if len(role.Rules) != 4 {
		t.Fatalf("Rules len: got %d, want %d", len(role.Rules), 4)
	}
	rule0 := role.Rules[0]
	wantVerbs := []string{"get", "list", "watch", "create", "update", "patch", "delete"}
	if !stringSliceEqual(rule0.Verbs, wantVerbs) {
		t.Errorf("Rules[0].Verbs: got %v, want %v", rule0.Verbs, wantVerbs)
	}
	if !stringSliceEqual(rule0.APIGroups, []string{""}) {
		t.Errorf("Rules[0].APIGroups: got %v, want [\"\"]", rule0.APIGroups)
	}
	if !stringSliceEqual(rule0.Resources, []string{"configmaps", "secrets"}) {
		t.Errorf("Rules[0].Resources: got %v, want [configmaps secrets]", rule0.Resources)
	}
	if !stringSliceEqual(rule0.ResourceNames, []string{config.ConfigMapPodTrafficManager}) {
		t.Errorf("Rules[0].ResourceNames: got %v, want [%s]", rule0.ResourceNames, config.ConfigMapPodTrafficManager)
	}
	// No-probe-pod CIDR detection: list pods (Rule 1) + create Service (Rule 2).
	if !stringSliceEqual(role.Rules[1].Resources, []string{"pods"}) {
		t.Errorf("Rules[1].Resources: got %v, want [pods]", role.Rules[1].Resources)
	}
	if !stringSliceEqual(role.Rules[1].Verbs, []string{"list"}) {
		t.Errorf("Rules[1].Verbs: got %v, want [list]", role.Rules[1].Verbs)
	}
	if !stringSliceEqual(role.Rules[2].Resources, []string{"services"}) {
		t.Errorf("Rules[2].Resources: got %v, want [services]", role.Rules[2].Resources)
	}
	// create (rejected-Service error trick) + list (infer CIDR from existing ClusterIPs).
	if !stringSliceEqual(role.Rules[2].Verbs, []string{"create", "list"}) {
		t.Errorf("Rules[2].Verbs: got %v, want [create list]", role.Rules[2].Verbs)
	}
	// Rule 3 must be the batch/jobs grant (node-agent Jobs); no pods/exec.
	if !stringSliceEqual(role.Rules[3].APIGroups, []string{"batch"}) {
		t.Errorf("Rules[3].APIGroups: got %v, want [batch]", role.Rules[3].APIGroups)
	}
	if !stringSliceEqual(role.Rules[3].Resources, []string{"jobs"}) {
		t.Errorf("Rules[3].Resources: got %v, want [jobs]", role.Rules[3].Resources)
	}
	for _, r := range role.Rules {
		for _, res := range r.Resources {
			if res == "pods/exec" {
				t.Errorf("manager Role must not grant pods/exec (dataplane verification is lease-based)")
			}
		}
	}
}

// TestGenRouteRole_Fields verifies that genRouteRole produces the expected name,
// namespace, and a single list/watch rule scoped to pods and services.
func TestGenRouteRole_Fields(t *testing.T) {
	const ns = "workload-ns"
	role := genRouteRole(ns)

	wantName := config.ConfigMapPodTrafficManager + "-route"
	if role.Name != wantName {
		t.Errorf("Name: got %q, want %q", role.Name, wantName)
	}
	if role.Namespace != ns {
		t.Errorf("Namespace: got %q, want %q", role.Namespace, ns)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("Rules len: got %d, want 1", len(role.Rules))
	}
	rule := role.Rules[0]
	if !stringSliceEqual(rule.Verbs, []string{"list", "watch"}) {
		t.Errorf("Rules[0].Verbs: got %v, want [list watch]", rule.Verbs)
	}
	if !stringSliceEqual(rule.APIGroups, []string{""}) {
		t.Errorf("Rules[0].APIGroups: got %v, want [\"\"]", rule.APIGroups)
	}
	if !stringSliceEqual(rule.Resources, []string{"pods", "services"}) {
		t.Errorf("Rules[0].Resources: got %v, want [pods services]", rule.Resources)
	}
	if len(rule.ResourceNames) != 0 {
		t.Errorf("Rules[0].ResourceNames: got %v, want empty (no filter)", rule.ResourceNames)
	}
}

// TestGenProxyRole_Fields verifies that genProxyRole produces the expected name,
// namespace, and a wildcard rule with all required verbs.
func TestGenProxyRole_Fields(t *testing.T) {
	const ns = "workload-ns"
	role := genProxyRole(ns)

	wantName := config.ConfigMapPodTrafficManager + "-proxy"
	if role.Name != wantName {
		t.Errorf("Name: got %q, want %q", role.Name, wantName)
	}
	if role.Namespace != ns {
		t.Errorf("Namespace: got %q, want %q", role.Namespace, ns)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("Rules len: got %d, want 1", len(role.Rules))
	}
	rule := role.Rules[0]
	wantVerbs := []string{"get", "list", "watch", "create", "delete", "patch", "update"}
	if !stringSliceEqual(rule.Verbs, wantVerbs) {
		t.Errorf("Rules[0].Verbs: got %v, want %v", rule.Verbs, wantVerbs)
	}
	if !stringSliceEqual(rule.APIGroups, []string{"*"}) {
		t.Errorf("Rules[0].APIGroups: got %v, want [*]", rule.APIGroups)
	}
	if !stringSliceEqual(rule.Resources, []string{"*"}) {
		t.Errorf("Rules[0].Resources: got %v, want [*]", rule.Resources)
	}
	if len(rule.ResourceNames) != 0 {
		t.Errorf("Rules[0].ResourceNames: got %v, want empty (no filter)", rule.ResourceNames)
	}
}

// TestGenRoleBinding_Fields verifies that genRoleBinding (single-namespace variant)
// produces the expected name, namespace, RoleRef, and Subject with the same namespace.
func TestGenRoleBinding_Fields(t *testing.T) {
	const ns = "mgr-ns"
	rb := genRoleBinding(ns)

	if rb.Name != config.ConfigMapPodTrafficManager {
		t.Errorf("Name: got %q, want %q", rb.Name, config.ConfigMapPodTrafficManager)
	}
	if rb.Namespace != ns {
		t.Errorf("Namespace: got %q, want %q", rb.Namespace, ns)
	}
	assertRoleRef(t, rb.RoleRef, "Role", config.ConfigMapPodTrafficManager)
	if len(rb.Subjects) != 1 {
		t.Fatalf("Subjects len: got %d, want 1", len(rb.Subjects))
	}
	assertSubject(t, rb.Subjects[0], config.ConfigMapPodTrafficManager, ns)
}

// TestGenRouteRoleBinding_Fields verifies that genRouteRoleBinding correctly sets
// the role namespace (for the object) and the SA namespace (for the Subject).
func TestGenRouteRoleBinding_Fields(t *testing.T) {
	const roleNS, saNS = "workload-ns", "mgr-ns"
	rb := genRouteRoleBinding(roleNS, saNS)

	wantName := config.ConfigMapPodTrafficManager + "-route"
	if rb.Name != wantName {
		t.Errorf("Name: got %q, want %q", rb.Name, wantName)
	}
	if rb.Namespace != roleNS {
		t.Errorf("Namespace: got %q, want %q", rb.Namespace, roleNS)
	}
	assertRoleRef(t, rb.RoleRef, "Role", wantName)
	if len(rb.Subjects) != 1 {
		t.Fatalf("Subjects len: got %d, want 1", len(rb.Subjects))
	}
	assertSubject(t, rb.Subjects[0], config.ConfigMapPodTrafficManager, saNS)
}

// TestGenProxyRoleBinding_Fields verifies that genProxyRoleBinding correctly sets
// the role namespace (for the object) and the SA namespace (for the Subject).
func TestGenProxyRoleBinding_Fields(t *testing.T) {
	const roleNS, saNS = "workload-ns", "mgr-ns"
	rb := genProxyRoleBinding(roleNS, saNS)

	wantName := config.ConfigMapPodTrafficManager + "-proxy"
	if rb.Name != wantName {
		t.Errorf("Name: got %q, want %q", rb.Name, wantName)
	}
	if rb.Namespace != roleNS {
		t.Errorf("Namespace: got %q, want %q", rb.Namespace, roleNS)
	}
	assertRoleRef(t, rb.RoleRef, "Role", wantName)
	if len(rb.Subjects) != 1 {
		t.Fatalf("Subjects len: got %d, want 1", len(rb.Subjects))
	}
	assertSubject(t, rb.Subjects[0], config.ConfigMapPodTrafficManager, saNS)
}

// TestGenRoleBinding_SameNamespace verifies the degenerate case where roleNamespace
// and saNamespace are identical — the common non-central deployment scenario.
func TestGenRoleBinding_SameNamespace(t *testing.T) {
	const ns = "same-ns"

	// genRoleBinding is the canonical single-arg form; its Subject.Namespace must
	// equal the object Namespace.
	rb := genRoleBinding(ns)
	if rb.Namespace != rb.Subjects[0].Namespace {
		t.Errorf("single-namespace genRoleBinding: object ns %q != subject ns %q",
			rb.Namespace, rb.Subjects[0].Namespace)
	}

	// Route and proxy variants with equal namespaces must behave the same way.
	for _, rb2 := range []*rbacv1.RoleBinding{
		genRouteRoleBinding(ns, ns),
		genProxyRoleBinding(ns, ns),
	} {
		if rb2.Namespace != rb2.Subjects[0].Namespace {
			t.Errorf("%s: object ns %q != subject ns %q",
				rb2.Name, rb2.Namespace, rb2.Subjects[0].Namespace)
		}
	}
}

// TestGenRoleBinding_CrossNamespace verifies that cross-namespace bindings keep the
// object in the workload namespace while the Subject points to the manager namespace.
func TestGenRoleBinding_CrossNamespace(t *testing.T) {
	const roleNS, saNS = "workload-ns", "kubevpn-ns"

	for _, rb := range []*rbacv1.RoleBinding{
		genRouteRoleBinding(roleNS, saNS),
		genProxyRoleBinding(roleNS, saNS),
	} {
		if rb.Namespace != roleNS {
			t.Errorf("%s: object ns: got %q, want %q", rb.Name, rb.Namespace, roleNS)
		}
		if rb.Subjects[0].Namespace != saNS {
			t.Errorf("%s: subject ns: got %q, want %q", rb.Name, rb.Subjects[0].Namespace, saNS)
		}
	}
}

// assertRoleRef is a test helper that checks all three fields of a RoleRef.
func assertRoleRef(t *testing.T, ref rbacv1.RoleRef, wantKind, wantName string) {
	t.Helper()
	if ref.APIGroup != "rbac.authorization.k8s.io" {
		t.Errorf("RoleRef.APIGroup: got %q, want %q", ref.APIGroup, "rbac.authorization.k8s.io")
	}
	if ref.Kind != wantKind {
		t.Errorf("RoleRef.Kind: got %q, want %q", ref.Kind, wantKind)
	}
	if ref.Name != wantName {
		t.Errorf("RoleRef.Name: got %q, want %q", ref.Name, wantName)
	}
}

// assertSubject is a test helper that checks Kind, Name, and Namespace of a Subject.
func assertSubject(t *testing.T, s rbacv1.Subject, wantName, wantNamespace string) {
	t.Helper()
	if s.Kind != "ServiceAccount" {
		t.Errorf("Subject.Kind: got %q, want ServiceAccount", s.Kind)
	}
	if s.Name != wantName {
		t.Errorf("Subject.Name: got %q, want %q", s.Name, wantName)
	}
	if s.Namespace != wantNamespace {
		t.Errorf("Subject.Namespace: got %q, want %q", s.Namespace, wantNamespace)
	}
}

// stringSliceEqual compares two string slices element-by-element.
func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
