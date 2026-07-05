package dns

import (
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func svc(name, clusterIP string) corev1.Service {
	s := corev1.Service{Spec: corev1.ServiceSpec{ClusterIP: clusterIP}}
	s.Name = name
	return s
}

// TestGenerateAppendHosts_NoDuplicates guards the O(n)->O(1)-membership dedup refactor: entries
// already known (c.Hosts / extra) and repeated services must each appear at most once.
func TestGenerateAppendHosts_NoDuplicates(t *testing.T) {
	c := &Config{
		Lock:  &sync.Mutex{},
		Hosts: []Entry{{IP: "10.0.0.1", Domain: "a"}},
	}
	services := []corev1.Service{
		svc("a", "10.0.0.1"), // same as an existing host entry -> must not duplicate
		svc("b", "10.0.0.2"),
		svc("b", "10.0.0.2"), // repeated within the input -> must not duplicate
		svc("c", "10.0.0.3"),
	}
	result := c.generateAppendHosts(services, []Entry{{IP: "10.0.0.1", Domain: "a"}})

	count := map[Entry]int{}
	for _, e := range result {
		count[e]++
	}
	for e, n := range count {
		if n > 1 {
			t.Errorf("duplicate entry %+v appears %d times", e, n)
		}
	}
	// b and c should be present exactly once each.
	for _, want := range []Entry{{IP: "10.0.0.2", Domain: "b"}, {IP: "10.0.0.3", Domain: "c"}} {
		if count[want] != 1 {
			t.Errorf("entry %+v: got %d, want 1", want, count[want])
		}
	}
}
