package cmds

import (
	"fmt"
	"net/http"
	"os"
	"runtime"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/upgrade"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdUpgrade(_ cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: i18n.T("Upgrade kubevpn client to latest version"),
		Long: templates.LongDesc(i18n.T(`
		Upgrade kubevpn client to latest version, automatically download and install latest kubevpn from GitHub.
		disconnect all from k8s cluster, leave all resources, remove all clone resource, and then,  
		upgrade local daemon grpc server to latest version.
		`)),
		Run: func(cmd *cobra.Command, args []string) {
			var client = http.DefaultClient
			if config.GitHubOAuthToken != "" {
				client = oauth2.NewClient(cmd.Context(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: config.GitHubOAuthToken, TokenType: "Bearer"}))
			}
			latestVersion, latestCommit, url, err := util.GetManifest(client, runtime.GOOS, runtime.GOARCH)
			if err != nil {
				log.Fatal(err)
			}
			err = upgrade.Main(cmd.Context(), client, latestVersion, latestCommit, url)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Fprint(os.Stdout, "Upgrade daemon...")
			err = daemon.StartupDaemon(cmd.Context())
			fmt.Fprint(os.Stdout, "done")
		},
	}
	return cmd
}
