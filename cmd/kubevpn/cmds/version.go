package cmds

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// --ldflags -X
var (
	OsArch    = ""
	BuildTime = ""
	Branch    = ""
)

func reformatDate(buildTime string) string {
	t, errTime := time.Parse(time.RFC3339Nano, buildTime)
	if errTime == nil {
		return t.Format("2006-01-02 15:04:05")
	}
	return buildTime
}

func CmdVersion(cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: i18n.T("Print the client version information"),
		Long: templates.LongDesc(i18n.T(`
		Print the client version information
		`)),
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("KubeVPN: CLI\n")
			fmt.Printf("    Version: %s\n", config.Version)
			fmt.Printf("    Daemon: %s\n", getDaemonVersion())
			fmt.Printf("    Image: %s\n", config.Image)
			fmt.Printf("    Branch: %s\n", Branch)
			fmt.Printf("    Git commit: %s\n", config.GitCommit)
			fmt.Printf("    Built time: %s\n", reformatDate(BuildTime))
			fmt.Printf("    Built OS/Arch: %s\n", OsArch)
			fmt.Printf("    Built Go version: %s\n", runtime.Version())
		},
	}
	return cmd
}

func init() {
	// Prefer version number inserted at build using --ldflags
	if config.Version == "" {
		if i, ok := debug.ReadBuildInfo(); ok {
			config.Version = i.Main.Version
		}
	}
}

func getDaemonVersion() string {
	cli := daemon.GetClient(false)
	if cli != nil {
		version, err := cli.Version(context.Background(), &rpc.VersionRequest{})
		if err == nil {
			return version.Version
		}
	}
	return "unknown"
}
