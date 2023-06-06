package cmds

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

// --ldflags -X
var (
	OsArch    = ""
	GitCommit = ""
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
		Short: "Print the client version information",
		Long:  `Print the client version information`,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("KubeVPN: CLI\n")
			fmt.Printf("    Version: %s\n", config.Version)
			fmt.Printf("    Image: %s\n", config.Image)
			fmt.Printf("    Branch: %s\n", Branch)
			fmt.Printf("    Git commit: %s\n", GitCommit)
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
