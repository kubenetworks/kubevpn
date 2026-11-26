package cmds

import (
	"os"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
)

// CmdLegacyProxy preserves the run/sync sidecar path without changing the
// public proxy command's parameters, behavior, or output.
func CmdLegacyProxy(f cmdutil.Factory) *cobra.Command {
	cmd := CmdProxy(f)
	cmd.Use = "legacy-proxy"
	cmd.Hidden = true
	runE := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := os.Setenv(daemon.LegacyProxyEnv, "true"); err != nil {
			return err
		}
		defer os.Unsetenv(daemon.LegacyProxyEnv)
		return runE(cmd, args)
	}
	return cmd
}
