package controlplane

import (
	"math"
	"strconv"
	"testing"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	log "github.com/sirupsen/logrus"
)

func TestNewProcessor(t *testing.T) {
	snapshotCache := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	logger := log.NewEntry(log.New())

	p := newProcessor(snapshotCache, logger)
	if p == nil {
		t.Fatal("newProcessor returned nil")
	}
	if p.cache == nil {
		t.Fatal("processor cache is nil")
	}
	if p.logger == nil {
		t.Fatal("processor logger is nil")
	}
	if p.expireCache == nil {
		t.Fatal("processor expireCache is nil")
	}
}

func TestNewVersion(t *testing.T) {
	snapshotCache := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	logger := log.NewEntry(log.New())

	p := newProcessor(snapshotCache, logger)
	initialVersion := p.version

	v1 := p.newVersion()
	expectedV1 := strconv.FormatInt(initialVersion+1, 10)
	if v1 != expectedV1 {
		t.Fatalf("first newVersion: got %s, want %s", v1, expectedV1)
	}

	v2 := p.newVersion()
	expectedV2 := strconv.FormatInt(initialVersion+2, 10)
	if v2 != expectedV2 {
		t.Fatalf("second newVersion: got %s, want %s", v2, expectedV2)
	}

	// Verify monotonic increment
	n1, _ := strconv.ParseInt(v1, 10, 64)
	n2, _ := strconv.ParseInt(v2, 10, 64)
	if n2 != n1+1 {
		t.Fatalf("versions not incrementing: v1=%d, v2=%d", n1, n2)
	}
}

func TestProcessFile_EmptyContent(t *testing.T) {
	snapshotCache := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	logger := log.NewEntry(log.New())

	p := newProcessor(snapshotCache, logger)

	err := p.ProcessFile(NotifyMessage{Content: ""})
	if err != nil {
		t.Fatalf("ProcessFile with empty content should succeed, got: %v", err)
	}
}

func TestProcessFile_ValidYAML(t *testing.T) {
	snapshotCache := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	logger := log.NewEntry(log.New())

	p := newProcessor(snapshotCache, logger)

	content := `
- namespace: default
  Uid: deployments.apps.reviews
  ports:
    - containerPort: 9080
      protocol: TCP
  rules:
    - headers:
        env: test
      localTunIPv4: "198.18.0.1"
      localTunIPv6: "2001:2::1"
      portMap:
        9080: "9080"
`
	err := p.ProcessFile(NotifyMessage{Content: content})
	if err != nil {
		t.Fatalf("ProcessFile with valid YAML failed: %v", err)
	}

	// Verify snapshot was set in the cache
	nodeID := "default.deployments.apps.reviews"
	snapshot, err := snapshotCache.GetSnapshot(nodeID)
	if err != nil {
		t.Fatalf("expected snapshot for node %s, got error: %v", nodeID, err)
	}
	if snapshot == nil {
		t.Fatal("snapshot is nil after ProcessFile")
	}
}

func TestProcessFile_CacheHit(t *testing.T) {
	snapshotCache := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	logger := log.NewEntry(log.New())

	p := newProcessor(snapshotCache, logger)

	content := `
- namespace: default
  Uid: deployments.apps.reviews
  ports:
    - containerPort: 9080
      protocol: TCP
  rules:
    - headers:
        env: test
      localTunIPv4: "198.18.0.1"
      localTunIPv6: "2001:2::1"
      portMap:
        9080: "9080"
`
	err := p.ProcessFile(NotifyMessage{Content: content})
	if err != nil {
		t.Fatalf("first ProcessFile failed: %v", err)
	}

	versionAfterFirst := p.version

	// Process same content again — should skip due to cache hit
	err = p.ProcessFile(NotifyMessage{Content: content})
	if err != nil {
		t.Fatalf("second ProcessFile failed: %v", err)
	}

	versionAfterSecond := p.version
	if versionAfterSecond != versionAfterFirst {
		t.Fatalf("version should not change on cache hit: before=%d, after=%d",
			versionAfterFirst, versionAfterSecond)
	}
}

func TestProcessFile_InvalidYAML(t *testing.T) {
	snapshotCache := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	logger := log.NewEntry(log.New())

	p := newProcessor(snapshotCache, logger)

	err := p.ProcessFile(NotifyMessage{Content: "not: [valid: yaml: {{"})
	if err == nil {
		t.Fatal("ProcessFile with invalid YAML should return error")
	}
}

func TestProcessFile_MultipleVirtuals(t *testing.T) {
	snapshotCache := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	logger := log.NewEntry(log.New())

	p := newProcessor(snapshotCache, logger)

	content := `
- namespace: default
  Uid: deployments.apps.frontend
  ports:
    - containerPort: 8080
      protocol: TCP
  rules:
    - headers:
        env: test
      localTunIPv4: "198.18.0.1"
      portMap:
        8080: "8080"
- namespace: staging
  Uid: deployments.apps.backend
  ports:
    - containerPort: 3000
      protocol: TCP
  rules:
    - headers:
        version: v2
      localTunIPv4: "198.18.0.2"
      portMap:
        3000: "3000"
`
	err := p.ProcessFile(NotifyMessage{Content: content})
	if err != nil {
		t.Fatalf("ProcessFile with multiple virtuals failed: %v", err)
	}

	// Verify snapshot for first virtual
	nodeID1 := "default.deployments.apps.frontend"
	snapshot1, err := snapshotCache.GetSnapshot(nodeID1)
	if err != nil {
		t.Fatalf("expected snapshot for node %s, got error: %v", nodeID1, err)
	}
	if snapshot1 == nil {
		t.Fatalf("snapshot is nil for %s", nodeID1)
	}

	// Verify snapshot for second virtual
	nodeID2 := "staging.deployments.apps.backend"
	snapshot2, err := snapshotCache.GetSnapshot(nodeID2)
	if err != nil {
		t.Fatalf("expected snapshot for node %s, got error: %v", nodeID2, err)
	}
	if snapshot2 == nil {
		t.Fatalf("snapshot is nil for %s", nodeID2)
	}

	// Verify they have different version strings (both incremented)
	if snapshot1.GetVersion("type.googleapis.com/envoy.config.listener.v3.Listener") ==
		snapshot2.GetVersion("type.googleapis.com/envoy.config.listener.v3.Listener") {
		t.Fatal("expected different snapshot versions for different virtuals")
	}
}

func TestProcessor_NewVersion_Overflow(t *testing.T) {
	snapshotCache := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	logger := log.NewEntry(log.New())

	p := newProcessor(snapshotCache, logger)

	// Set version to MaxInt64 to test wrapping
	p.version = math.MaxInt64

	v1 := p.newVersion()
	// After wrapping, version should be 1 (reset to 0 then incremented)
	expected := strconv.FormatInt(1, 10)
	if v1 != expected {
		t.Fatalf("expected version %q after overflow, got %q", expected, v1)
	}

	// Verify subsequent versions continue incrementing normally
	v2 := p.newVersion()
	expected2 := strconv.FormatInt(2, 10)
	if v2 != expected2 {
		t.Fatalf("expected version %q after overflow+1, got %q", expected2, v2)
	}

	// Confirm internal state is consistent
	if p.version != 2 {
		t.Fatalf("expected internal version 2, got %d", p.version)
	}
}
