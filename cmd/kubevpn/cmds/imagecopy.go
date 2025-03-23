package cmds

import (
	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util/regctl"
)

func CmdImageCopy(_ cmdutil.Factory) *cobra.Command {
	var imageCmd = &cobra.Command{
		Use:   "image <cmd>",
		Short: "copy images",
	}

	copyCmd := &cobra.Command{
		Use:     "copy <src_image_ref> <dst_image_ref>",
		Aliases: []string{"cp"},
		Short:   "copy or re-tag image",
		Long: `Copy or re-tag an image. This works between registries and only pulls layers
that do not exist at the target. In the same registry it attempts to mount
the layers between repositories. And within the same repository it only
sends the manifest with the new tag.`,
		Example: `
# copy an image
kubevpn image copy ghcr.io/kubenetworks/kubevpn:latest registry.example.org/kubevpn/kubevpn:latest

# re-tag an image
kubevpn image copy ghcr.io/kubenetworks/kubevpn:latest ghcr.io/kubenetworks/kubevpn:v2.3.4`,
		Args: cobra.MatchAll(cobra.ExactArgs(2), cobra.OnlyValidArgs),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			plog.InitLoggerForClient()
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			err := regctl.TransferImageWithRegctl(cmd.Context(), args[0], args[1])
			return err
		},
	}
	imageCmd.AddCommand(copyCmd)
	return imageCmd
}
