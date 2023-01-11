package cmds

import (
	"net/http"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"

	"github.com/wencaiwulue/kubevpn/pkg/upgrade"
)

// GitHubOAuthToken
// --ldflags -X
var (
	GitHubOAuthToken = ""
)

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade KubeVPN version",
	Long:  `Upgrade KubeVPN version, automatically download latest KubeVPN from GitHub`,
	Run: func(cmd *cobra.Command, args []string) {
		var client = http.DefaultClient
		if GitHubOAuthToken != "" {
			client = oauth2.NewClient(cmd.Context(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: GitHubOAuthToken, TokenType: "Bearer"}))
		}
		err := upgrade.Main(Version, client)
		if err != nil {
			log.Fatal(err)
		}
	},
}

func init() {
	RootCmd.AddCommand(upgradeCmd)
}
