package cmds

import (
	"fmt"
	"net/http"
	"os"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/upgrade"
)

// GitHubOAuthToken
// --ldflags -X
var (
	GitHubOAuthToken = ""
)

func CmdUpgrade(_ cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade KubeVPN version",
		Long:  `Upgrade KubeVPN version, automatically download latest KubeVPN from GitHub`,
		Run: func(cmd *cobra.Command, args []string) {
			var client = http.DefaultClient
			if GitHubOAuthToken != "" {
				client = oauth2.NewClient(cmd.Context(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: GitHubOAuthToken, TokenType: "Bearer"}))
			}
			err := upgrade.Main(config.Version, config.GitCommit, client)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Fprint(os.Stdout, "done")
		},
	}
	return cmd
}
