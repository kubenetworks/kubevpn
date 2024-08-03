package cmds

import (
	"cmp"
	"encoding/json"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/printers"
	cmdget "k8s.io/kubectl/pkg/cmd/get"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func CmdGet(f cmdutil.Factory) *cobra.Command {
	var printFlags = cmdget.NewGetPrintFlags()
	cmd := &cobra.Command{
		Use:    "get",
		Hidden: true,
		Short:  i18n.T("Get cluster resources which connected"),
		Long:   templates.LongDesc(i18n.T(`Get cluster resources which connected`)),
		Example: templates.Examples(i18n.T(`
		# Get resource to k8s cluster network
		kubevpn get pods

		# Get api-server behind of bastion host or ssh jump host
		kubevpn get deployment --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# It also support ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn get service --ssh-alias <alias>
		`)),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// startup daemon process and sudo process
			return daemon.StartupDaemon(cmd.Context())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, _, err := f.ToRawKubeConfigLoader().Namespace()
			if err != nil {
				return err
			}
			client, err := daemon.GetClient(true).Get(
				cmd.Context(),
				&rpc.GetRequest{
					Namespace: ns,
					Resource:  args[0],
				},
			)
			if err != nil {
				return err
			}
			w := printers.GetNewTabWriter(os.Stdout)
			var toPrinter = func() (printers.ResourcePrinterFunc, error) {
				var flags = printFlags.Copy()
				_ = flags.EnsureWithNamespace()
				printer, err := flags.ToPrinter()
				if err != nil {
					return nil, err
				}
				printer, err = printers.NewTypeSetter(scheme.Scheme).WrapToPrinter(printer, nil)
				if err != nil {
					return nil, err
				}
				outputOption := cmd.Flags().Lookup("output").Value.String()
				if strings.Contains(outputOption, "custom-columns") || outputOption == "yaml" || strings.Contains(outputOption, "json") {
				} else {
					printer = &cmdget.TablePrinter{Delegate: printer}
				}
				return printer.PrintObj, nil
			}
			var list []*v1.PartialObjectMetadata
			for _, m := range client.Metadata {
				var data v1.PartialObjectMetadata
				err = json.Unmarshal([]byte(m), &data)
				if err != nil {
					continue
				}
				list = append(list, &data)
			}
			slices.SortStableFunc(list, func(a, b *v1.PartialObjectMetadata) int {
				compare := cmp.Compare(a.GetNamespace(), b.GetNamespace())
				if compare == 0 {
					return cmp.Compare(a.GetName(), b.GetName())
				}
				return compare
			})
			for _, m := range list {
				printer, err := toPrinter()
				if err != nil {
					return err
				}
				_ = printer.PrintObj(m, w)
			}
			return w.Flush()
		},
	}
	printFlags.AddFlags(cmd)
	return cmd
}
