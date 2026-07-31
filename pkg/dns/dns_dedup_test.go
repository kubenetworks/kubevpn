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

// TestLineHasDomain pins the whole-field domain match that replaced the old
// strings.Contains check. The hosts keyword "Added by KubeVPN" and the tun name share
// many letters with short domains, so a substring match would match the marker text
// itself — only a whitespace-delimited whole-field match is correct.
//
// lineHasDomain reports whether domain appears as ANY whitespace-delimited field on
// the line. Its only caller guards with `strings.Contains(line, HostsKeyword)` first,
// so it runs only on managed lines; the field match then correctly distinguishes "this
// domain is present" from "this domain's letters appear somewhere on the line".
func TestLineHasDomain(t *testing.T) {
	const managed = "10.0.0.2\tmyservice\t\t# For dev kubevpn-tun Added by KubeVPN"
	cases := []struct {
		line   string
		domain string
		want   bool
	}{
		{managed, "myservice", true},  // the domain field
		{managed, "MyService", true},  // case-insensitive
		{managed, "b", false},         // single letter — substring of "by"/"KubeVPN", NOT a field — the bug case
		{managed, "vpn", false},       // substring of "KubeVPN"/"kubevpn-tun", NOT a field
		{managed, "Kube", false},      // substring of "KubeVPN", NOT a field
		{managed, "10.0.0.2", true},  // IP is a field (match is field-agnostic; caller filters by keyword)
		{"# comment with myservice inside", "myservice", true}, // myservice IS a field here — caller's keyword guard excludes unmanaged lines
		{"", "anything", false},
	}
	for _, c := range cases {
		got := lineHasDomain(c.line, c.domain)
		if got != c.want {
			t.Errorf("lineHasDomain(%q, %q) = %v, want %v", c.line, c.domain, got, c.want)
		}
	}
}


