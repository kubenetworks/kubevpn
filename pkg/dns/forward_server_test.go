package dns

import (
	"testing"
)

func TestFix(t *testing.T) {
	domain := "authors"
	search := []string{"default.svc.cluster.local", "svc.cluster.local", "cluster.local"}
	result := fix(domain, search)

	expected := []string{
		"authors.",
		"authors.default.svc.cluster.local.",
		"authors.svc.cluster.local.",
		"authors.cluster.local.",
	}
	if len(result) != len(expected) {
		t.Fatalf("fix(%q) returned %d results, want %d", domain, len(result), len(expected))
	}
	for i, got := range result {
		if got != expected[i] {
			t.Errorf("fix(%q)[%d] = %q, want %q", domain, i, got, expected[i])
		}
	}
}

func TestFixTrailingDots(t *testing.T) {
	// Name with trailing dots should have them stripped before appending suffixes
	result := fix("myservice...", []string{"ns.svc.cluster.local"})

	expected := []string{
		"myservice.",
		"myservice.ns.svc.cluster.local.",
	}
	if len(result) != len(expected) {
		t.Fatalf("fix with trailing dots returned %d results, want %d", len(result), len(expected))
	}
	for i, got := range result {
		if got != expected[i] {
			t.Errorf("result[%d] = %q, want %q", i, got, expected[i])
		}
	}
}

func TestFixEmptySearch(t *testing.T) {
	result := fix("example.com", nil)
	if len(result) != 1 {
		t.Fatalf("fix with empty search returned %d results, want 1", len(result))
	}
	if result[0] != "example.com." {
		t.Errorf("result[0] = %q, want %q", result[0], "example.com.")
	}
}

func TestFixFQDN(t *testing.T) {
	// Already an FQDN with trailing dot — should be treated the same
	result := fix("myservice.default.svc.cluster.local.", []string{"default.svc.cluster.local"})
	expected := []string{
		"myservice.default.svc.cluster.local.",
		"myservice.default.svc.cluster.local.default.svc.cluster.local.",
	}
	if len(result) != len(expected) {
		t.Fatalf("fix FQDN returned %d results, want %d", len(result), len(expected))
	}
	if result[0] != expected[0] {
		t.Errorf("result[0] = %q, want %q", result[0], expected[0])
	}
}

func TestFixMultipleDots(t *testing.T) {
	// Compound name with embedded dots
	result := fix("mongo-headless.mongodb", []string{"default.svc.cluster.local"})
	expected := []string{
		"mongo-headless.mongodb.",
		"mongo-headless.mongodb.default.svc.cluster.local.",
	}
	if len(result) != len(expected) {
		t.Fatalf("fix compound name returned %d results, want %d", len(result), len(expected))
	}
	for i, got := range result {
		if got != expected[i] {
			t.Errorf("result[%d] = %q, want %q", i, got, expected[i])
		}
	}
}

func TestFixSingleCharName(t *testing.T) {
	result := fix("a", []string{"ns"})
	expected := []string{"a.", "a.ns."}
	if len(result) != len(expected) {
		t.Fatalf("got %d results, want %d", len(result), len(expected))
	}
	for i, got := range result {
		if got != expected[i] {
			t.Errorf("result[%d] = %q, want %q", i, got, expected[i])
		}
	}
}

func TestFixFirstResultIsBareName(t *testing.T) {
	// The first result should always be just the name with a trailing dot
	search := []string{"a", "b", "c"}
	result := fix("svc", search)
	if result[0] != "svc." {
		t.Errorf("first result = %q, want bare name %q", result[0], "svc.")
	}
	if len(result) != 4 {
		t.Errorf("expected 4 results (1 bare + 3 suffixed), got %d", len(result))
	}
}
