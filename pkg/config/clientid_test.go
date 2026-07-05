package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetClientID_StableAndPersisted(t *testing.T) {
	id1 := GetClientID()
	if len(id1) != 12 {
		t.Fatalf("expected 12-char client id, got %q (len %d)", id1, len(id1))
	}
	// Stable across calls (cached + persisted).
	if id2 := GetClientID(); id2 != id1 {
		t.Errorf("client id not stable: %q != %q", id1, id2)
	}
	// Persisted to ~/.kubevpn/client_id, prefix matches the returned id.
	data, err := os.ReadFile(filepath.Join(homePath, ClientIDFile))
	if err != nil {
		t.Fatalf("client id not persisted: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(data)), id1) {
		t.Errorf("persisted id %q does not start with returned id %q", strings.TrimSpace(string(data)), id1)
	}
}
