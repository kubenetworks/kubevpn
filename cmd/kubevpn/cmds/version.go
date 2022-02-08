package cmds

import (
	"fmt"
	"github.com/spf13/cobra"
	"runtime"
	"runtime/debug"
	"time"
)

// --ldflags -X
var (
	Version   = ""
	OsArch    = ""
	GitCommit = ""
	BuildTime = ""
	Branch    = ""
)

func reformatDate(buildTime string) string {
	t, errTime := time.Parse(time.RFC3339Nano, buildTime)
	if errTime == nil {
		return t.Format(time.ANSIC)
	}
	return buildTime
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number of KubeVPN",
	Long:  `This is the version of KubeVPN`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("KubeVPN: CLI\n")
		fmt.Printf("    Version: %s\n", Version)
		fmt.Printf("    Branch: %s\n", Branch)
		fmt.Printf("    Git commit: %s\n", GitCommit)
		fmt.Printf("    Built time: %s\n", reformatDate(BuildTime))
		fmt.Printf("    Built OS/Arch: %s\n", OsArch)
		fmt.Printf("    Built Go version: %s\n", runtime.Version())
	},
}

func init() {
	RootCmd.AddCommand(versionCmd)
	// Prefer version number inserted at build using --ldflags
	if Version == "" {
		if i, ok := debug.ReadBuildInfo(); ok {
			Version = i.Main.Version
		}
	}
}
