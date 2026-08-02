package xds

import (
	"testing"
)

func TestParsePortMap_Empty(t *testing.T) {
	// nil PortMap
	r := &Rule{PortMap: nil}
	result := r.ParsePortMap()
	if len(result) != 0 {
		t.Fatalf("nil PortMap: expected 0 mappings, got %d", len(result))
	}

	// empty PortMap
	r = &Rule{PortMap: map[int32]string{}}
	result = r.ParsePortMap()
	if len(result) != 0 {
		t.Fatalf("empty PortMap: expected 0 mappings, got %d", len(result))
	}
}

func TestParsePortMap_MultipleEntries(t *testing.T) {
	r := &Rule{
		PortMap: map[int32]string{
			8080: "8080",
			9090: "29090:19090",
			53:   "53",
		},
	}
	result := r.ParsePortMap()
	if len(result) != 3 {
		t.Fatalf("expected 3 mappings, got %d", len(result))
	}

	// Index by ContainerPort for deterministic checks (map iteration order is random).
	byContainer := make(map[int32]PortMapping, len(result))
	for _, pm := range result {
		byContainer[pm.ContainerPort] = pm
	}

	// 8080 -> plain "8080": EnvoyPort=8080, LocalPort=containerPort=8080
	pm := byContainer[8080]
	if pm.EnvoyPort != 8080 {
		t.Fatalf("port 8080: EnvoyPort want 8080, got %d", pm.EnvoyPort)
	}
	if pm.LocalPort != 8080 {
		t.Fatalf("port 8080: LocalPort want 8080, got %d", pm.LocalPort)
	}

	// 9090 -> "29090:19090": EnvoyPort=29090, LocalPort=19090
	pm = byContainer[9090]
	if pm.EnvoyPort != 29090 {
		t.Fatalf("port 9090: EnvoyPort want 29090, got %d", pm.EnvoyPort)
	}
	if pm.LocalPort != 19090 {
		t.Fatalf("port 9090: LocalPort want 19090, got %d", pm.LocalPort)
	}

	// 53 -> plain "53": EnvoyPort=53, LocalPort=containerPort=53
	pm = byContainer[53]
	if pm.EnvoyPort != 53 {
		t.Fatalf("port 53: EnvoyPort want 53, got %d", pm.EnvoyPort)
	}
	if pm.LocalPort != 53 {
		t.Fatalf("port 53: LocalPort want 53, got %d", pm.LocalPort)
	}
}

func TestParsePortMap_ColonWithInvalidFirst(t *testing.T) {
	// First number in "abc:9090" is not a valid integer — EnvoyPort should be 0.
	r := &Rule{
		PortMap: map[int32]string{
			8080: "abc:9090",
		},
	}
	result := r.ParsePortMap()
	if len(result) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(result))
	}
	pm := result[0]
	if pm.ContainerPort != 8080 {
		t.Fatalf("ContainerPort want 8080, got %d", pm.ContainerPort)
	}
	if pm.EnvoyPort != 0 {
		t.Fatalf("EnvoyPort want 0 for invalid input, got %d", pm.EnvoyPort)
	}
	if pm.LocalPort != 9090 {
		t.Fatalf("LocalPort want 9090, got %d", pm.LocalPort)
	}
}

func TestParsePortMap_ZeroPort(t *testing.T) {
	// Plain "0" — EnvoyPort=0, LocalPort=containerPort
	r := &Rule{
		PortMap: map[int32]string{
			443: "0",
		},
	}
	result := r.ParsePortMap()
	if len(result) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(result))
	}
	pm := result[0]
	if pm.ContainerPort != 443 {
		t.Fatalf("ContainerPort want 443, got %d", pm.ContainerPort)
	}
	if pm.EnvoyPort != 0 {
		t.Fatalf("EnvoyPort want 0, got %d", pm.EnvoyPort)
	}
	if pm.LocalPort != 443 {
		t.Fatalf("LocalPort want containerPort 443, got %d", pm.LocalPort)
	}

	// Colon format "0:0" — both should be 0
	r = &Rule{
		PortMap: map[int32]string{
			443: "0:0",
		},
	}
	result = r.ParsePortMap()
	if len(result) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(result))
	}
	pm = result[0]
	if pm.EnvoyPort != 0 {
		t.Fatalf("EnvoyPort want 0, got %d", pm.EnvoyPort)
	}
	if pm.LocalPort != 0 {
		t.Fatalf("LocalPort want 0, got %d", pm.LocalPort)
	}
}
