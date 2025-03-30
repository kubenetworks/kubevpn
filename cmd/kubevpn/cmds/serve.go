package cmds

import (
	"math/rand"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/automaxprocs/maxprocs"
	glog "gvisor.dev/gvisor/pkg/log"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func CmdServe(_ cmdutil.Factory) *cobra.Command {
	var route = &core.Route{}
	cmd := &cobra.Command{
		Use:    "serve",
		Hidden: true,
		Short:  "Server side, startup traffic manager, forward inbound and outbound traffic",
		Long: templates.LongDesc(i18n.T(`
		Server side, startup traffic manager, forward inbound and outbound traffic.
		`)),
		Example: templates.Examples(i18n.T(`
        # serve node
        kubevpn serve -L "tcp://:10800" -L "tun://127.0.0.1:8422?net=198.19.0.123/32"
		`)),
		PreRun: func(*cobra.Command, []string) {
			runtime.GOMAXPROCS(0)
			go util.StartupPProfForServer(config.PProfPort)
			glog.SetTarget(plog.ServerEmitter{Writer: &glog.Writer{Next: os.Stderr}})
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			rand.Seed(time.Now().UnixNano())
			_, _ = maxprocs.Set(maxprocs.Logger(nil))
			ctx := cmd.Context()
			servers, err := handler.Parse(*route)
			if err != nil {
				plog.G(ctx).Errorf("Parse server failed: %v", err)
				return err
			}
			return handler.Run(ctx, servers)
		},
	}
	cmd.Flags().StringArrayVarP(&route.ServeNodes, "node", "L", []string{}, "Startup node server. eg: tcp://localhost:1080")
	cmd.Flags().StringVarP(&route.ChainNode, "chain", "F", "", "Forward chain. eg: tcp://192.168.1.100:2345")
	cmd.Flags().BoolVar(&config.Debug, "debug", false, "Enable debug log or not")
	return cmd
}
