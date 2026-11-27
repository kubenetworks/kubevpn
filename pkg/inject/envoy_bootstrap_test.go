package inject

import (
	"context"
	"strings"
	"testing"
)

func TestRenderEnvoyBootstrapWithAdmin(t *testing.T) {
	b, err := RenderEnvoyBootstrapWithAdmin(context.Background(), "kubevpn-traffic-manager.kubevpn:10801", 25001)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "port_value: 25001") {
		t.Fatalf("admin port not rendered: %s", b)
	}
	if strings.Contains(string(b), "port_value: 9003") {
		t.Fatalf("default admin port still present: %s", b)
	}
}
