package action

import (
	"context"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func TestVersion_ReturnsConfigVersion(t *testing.T) {
	svr := &Server{}
	resp, err := svr.Version(context.Background(), &rpc.VersionRequest{})
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if resp.Version != config.Version {
		t.Errorf("expected version %q, got %q", config.Version, resp.Version)
	}
}
