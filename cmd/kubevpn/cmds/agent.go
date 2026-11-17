package cmds

import (
	"fmt"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	pkgagent "github.com/wencaiwulue/kubevpn/v2/pkg/agent"
)

// CmdAgent is the node-hosted traffic agent entrypoint (docs/55). Hidden: it
// is invoked by kubevpn-nodeagent Jobs, not by users.
func CmdAgent(f cmdutil.Factory) *cobra.Command {
	var podUID, procRoot string
	cmd := &cobra.Command{
		Use:    "agent --pod-uid <uid> -- <argv...>",
		Short:  i18n.T("Node-hosted traffic agent: enter a pod's netns and exec a payload"),
		Long:   templates.LongDesc(i18n.T(`Node-hosted traffic agent: enter a pod's netns and exec a payload`)),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if podUID == "" {
				return fmt.Errorf("--pod-uid is required")
			}
			pid, err := pkgagent.FindNetnsPID(procRoot, podUID)
			if err != nil {
				return err
			}
			if err := pkgagent.EnterNetns(procRoot, pid); err != nil {
				return err
			}
			return pkgagent.Exec(args)
		},
		DisableFlagsInUseLine: true,
	}
	cmd.Flags().StringVar(&podUID, "pod-uid", "", "UID of the target pod")
	cmd.Flags().StringVar(&procRoot, "proc-root", "/host/proc", "Path to the mounted host /proc")
	return cmd
}
