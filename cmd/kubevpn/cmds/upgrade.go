package cmds

import (
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
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
		RunE: func(cmd *cobra.Command, args []string) error {
			const (
				envLatestUrl = "KUBEVPN_LATEST_VERSION_URL"
			)
			util.InitLoggerForClient(false)
			var client = http.DefaultClient
			if config.GitHubOAuthToken != "" {
				client = oauth2.NewClient(cmd.Context(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: config.GitHubOAuthToken, TokenType: "Bearer"}))
			}
			var url = os.Getenv(envLatestUrl)
			if url == "" {
				var latestVersion string
				var needsUpgrade bool
				var err error
				url, latestVersion, needsUpgrade, err = upgrade.NeedsUpgrade(cmd.Context(), client, config.Version)
				if err != nil {
					return err
				}
				if !needsUpgrade {
					_, _ = fmt.Fprintln(os.Stdout, fmt.Sprintf("Already up to date, don't needs to upgrade, version: %s", latestVersion))
					return nil
				}
				_, _ = fmt.Fprintln(os.Stdout, fmt.Sprintf("Current version is: %s less than latest version: %s, needs to upgrade", config.Version, latestVersion))
				_ = os.Setenv(envLatestUrl, url)
				_ = quit(cmd.Context(), false)
				_ = quit(cmd.Context(), true)
			}
			return upgrade.Main(cmd.Context(), client, url)
		},
	}
	return cmd
}
