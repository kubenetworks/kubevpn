package action

import (
	"context"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func TestServer_Upgrade(t *testing.T) {
	svr := &Server{}
	ctx := context.Background()

	orig := config.Version
	defer func() { config.Version = orig }()
	config.Version = "v2.0.0"

	cases := []struct {
		name          string
		daemonVersion string
		clientVersion string
		wantUpgrade   bool
	}{
		{"client newer -> upgrade", "v2.0.0", "v2.1.0", true},
		{"client equal -> no upgrade", "v2.0.0", "v2.0.0", false},
		{"client older -> no upgrade", "v2.0.0", "v1.9.0", false},
		// Dev/CI builds carry non-semver versions; must be tolerated, not error.
		{"unparseable client -> no upgrade, no error", "v2.0.0", "dev-378749d", false},
		{"unparseable daemon -> no upgrade, no error", "latest", "v2.0.0", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			config.Version = c.daemonVersion
			resp, err := svr.Upgrade(ctx, &rpc.UpgradeRequest{ClientVersion: c.clientVersion})
			if err != nil {
				t.Fatalf("Upgrade returned error (must be tolerant): %v", err)
			}
			if resp.NeedUpgrade != c.wantUpgrade {
				t.Fatalf("NeedUpgrade = %v, want %v", resp.NeedUpgrade, c.wantUpgrade)
			}
		})
	}
}
