package dns

import (
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestEntryList2String(t *testing.T) {
	c := &Config{
		TunName: "tun0",
		Lock:    &sync.Mutex{},
	}

	entries := []Entry{
		{IP: "10.0.0.1", Domain: "my-service"},
		{IP: "10.0.0.2", Domain: "my-other-service"},
	}
	result := c.entryList2String(entries)

	// Each entry should produce a line with IP, domain, and the device keyword
	keyword := config.HostsKeyword
	if !strings.Contains(result, "10.0.0.1") {
		t.Error("result should contain IP 10.0.0.1")
	}
	if !strings.Contains(result, "my-service") {
		t.Error("result should contain domain my-service")
	}
	if !strings.Contains(result, "10.0.0.2") {
		t.Error("result should contain IP 10.0.0.2")
	}
	if !strings.Contains(result, "my-other-service") {
		t.Error("result should contain domain my-other-service")
	}
	if !strings.Contains(result, keyword) {
		t.Errorf("result should contain hosts keyword %q", keyword)
	}
	if !strings.Contains(result, "tun0") {
		t.Error("result should contain TUN device name")
	}
}

func TestEntryList2StringEmpty(t *testing.T) {
	c := &Config{
		TunName: "tun0",
		Lock:    &sync.Mutex{},
	}

	result := c.entryList2String(nil)
	if strings.TrimSpace(result) != "" {
		t.Errorf("empty entry list should produce empty string, got %q", result)
	}
}

func TestEntryList2StringDeviceKeyword(t *testing.T) {
	c := &Config{
		TunName: "utun5",
		Lock:    &sync.Mutex{},
	}

	entries := []Entry{{IP: "192.168.1.1", Domain: "test"}}
	result := c.entryList2String(entries)

	expected := "# For dev utun5 " + config.HostsKeyword
	if !strings.Contains(result, expected) {
		t.Errorf("result should contain device keyword %q, got:\n%s", expected, result)
	}
}

func TestEntryEquality(t *testing.T) {
	e1 := Entry{IP: "10.0.0.1", Domain: "svc-a"}
	e2 := Entry{IP: "10.0.0.1", Domain: "svc-a"}
	e3 := Entry{IP: "10.0.0.1", Domain: "svc-b"}
	e4 := Entry{IP: "10.0.0.2", Domain: "svc-a"}

	if e1 != e2 {
		t.Error("identical entries should be equal")
	}
	if e1 == e3 {
		t.Error("entries with different domains should not be equal")
	}
	if e1 == e4 {
		t.Error("entries with different IPs should not be equal")
	}
}

func TestEntryList2StringMultipleLines(t *testing.T) {
	c := &Config{
		TunName: "tun0",
		Lock:    &sync.Mutex{},
	}

	entries := []Entry{
		{IP: "10.0.0.1", Domain: "svc1"},
		{IP: "10.0.0.2", Domain: "svc2"},
		{IP: "10.0.0.3", Domain: "svc3"},
	}
	result := c.entryList2String(entries)

	// Each entry should be on its own line
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d: %q", len(lines), result)
	}
}

func TestGenerateAppendHosts_SkipsKubernetes(t *testing.T) {
	c := &Config{Lock: &sync.Mutex{}}
	services := []corev1.Service{
		{Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.1"}},
	}
	services[0].Name = "kubernetes"
	result := c.generateAppendHosts(services, nil)
	for _, e := range result {
		if e.Domain == "kubernetes" {
			t.Error("kubernetes service should be skipped")
		}
	}
}

func TestGenerateAppendHosts_AddsService(t *testing.T) {
	c := &Config{Lock: &sync.Mutex{}}
	services := []corev1.Service{
		{Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.10"}},
	}
	services[0].Name = "my-svc"
	result := c.generateAppendHosts(services, nil)
	found := false
	for _, e := range result {
		if e.Domain == "my-svc" && e.IP == "10.96.0.10" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected my-svc entry, got %v", result)
	}
}

func TestGenerateAppendHosts_MergesExistingHosts(t *testing.T) {
	c := &Config{
		Lock:  &sync.Mutex{},
		Hosts: []Entry{{IP: "10.0.0.1", Domain: "existing"}},
	}
	result := c.generateAppendHosts(nil, nil)
	found := false
	for _, e := range result {
		if e.Domain == "existing" {
			found = true
		}
	}
	if !found {
		t.Error("existing hosts should be preserved")
	}
}

func TestGenerateAppendHosts_EmptyInput(t *testing.T) {
	c := &Config{Lock: &sync.Mutex{}}
	result := c.generateAppendHosts(nil, nil)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %v", result)
	}
}

func TestGenerateAppendHosts_SkipsNilIP(t *testing.T) {
	c := &Config{Lock: &sync.Mutex{}}
	services := []corev1.Service{
		{Spec: corev1.ServiceSpec{ClusterIP: ""}},
	}
	services[0].Name = "headless"
	result := c.generateAppendHosts(services, nil)
	for _, e := range result {
		if e.Domain == "headless" {
			t.Error("service with empty ClusterIP should be skipped")
		}
	}
}
