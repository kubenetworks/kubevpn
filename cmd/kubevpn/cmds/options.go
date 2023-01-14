package cmds

import (
	"os"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

var (
	optionsExample = templates.Examples(i18n.T(`
		# Print flags inherited by all commands
		kubectl options`))
)

func CmdOptions(cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "options",
		Short:   i18n.T("Print the list of flags inherited by all commands"),
		Long:    i18n.T("Print the list of flags inherited by all commands"),
		Example: optionsExample,
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Usage()
		},
	}

	cmd.SetOut(os.Stdout)

	templates.UseOptionsTemplates(cmd)
	return cmd
}
