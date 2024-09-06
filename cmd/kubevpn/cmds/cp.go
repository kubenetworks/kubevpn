package cmds

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/completion"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/cp"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
)

var cpExample = templates.Examples(i18n.T(`
		# !!!Important Note!!!
		# Requires that the 'tar' binary is present in your container
		# image.  If 'tar' is not present, 'kubectl cp' will fail.
		#
		# For advanced use cases, such as symlinks, wildcard expansion or
		# file mode preservation, consider using 'kubectl exec'.

		# Copy /tmp/foo local file to /tmp/bar in a remote pod in namespace <some-namespace>
		tar cf - /tmp/foo | kubectl exec -i -n <some-namespace> <some-pod> -- tar xf - -C /tmp/bar

		# Copy /tmp/foo from a remote pod to /tmp/bar locally
		kubectl exec -n <some-namespace> <some-pod> -- tar cf - /tmp/foo | tar xf - -C /tmp/bar

		# Copy /tmp/foo_dir local directory to /tmp/bar_dir in a remote pod in the default namespace
		kubectl cp /tmp/foo_dir <some-pod>:/tmp/bar_dir

		# Copy /tmp/foo local file to /tmp/bar in a remote pod in a specific container
		kubectl cp /tmp/foo <some-pod>:/tmp/bar -c <specific-container>

		# Copy /tmp/foo local file to /tmp/bar in a remote pod in namespace <some-namespace>
		kubectl cp /tmp/foo <some-namespace>/<some-pod>:/tmp/bar

		# Copy /tmp/foo from a remote pod to /tmp/bar locally
		kubectl cp <some-namespace>/<some-pod>:/tmp/foo /tmp/bar

		# copy reverse proxy api-server behind of bastion host or ssh jump host
		kubevpn cp deployment/productpage --ssh-addr 192.168.1.100:22 --ssh-username root --ssh-keyfile ~/.ssh/ssh.pem

		# It also supports ProxyJump, like
		┌──────┐     ┌──────┐     ┌──────┐     ┌──────┐                 ┌────────────┐
		│  pc  ├────►│ ssh1 ├────►│ ssh2 ├────►│ ssh3 ├─────►... ─────► │ api-server │
		└──────┘     └──────┘     └──────┘     └──────┘                 └────────────┘
		kubevpn cp deployment/productpage:/tmp/foo /tmp/bar --ssh-alias <alias>

		# Support ssh auth GSSAPI
        kubevpn cp deployment/productpage:/tmp/foo /tmp/bar --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-keytab /path/to/keytab
        kubevpn cp deployment/productpage:/tmp/foo /tmp/bar --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-cache /path/to/cache
        kubevpn cp deployment/productpage:/tmp/foo /tmp/bar --ssh-addr <HOST:PORT> --ssh-username <USERNAME> --gssapi-password <PASSWORD>
`,
))

func CmdCp(f cmdutil.Factory) *cobra.Command {
	o := cp.NewCopyOptions(genericiooptions.IOStreams{
		In:     os.Stdin,
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	})
	var sshConf = &pkgssh.SshConfig{}
	cmd := &cobra.Command{
		Use:                   "cp <file-spec-src> <file-spec-dest>",
		DisableFlagsInUseLine: true,
		Hidden:                true,
		Short:                 i18n.T("Copy files and directories to and from containers"),
		Long:                  i18n.T("Copy files and directories to and from containers. Different between kubectl cp is it will de-reference symbol link."),
		Example:               cpExample,
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			cmdutil.CheckErr(pkgssh.SshJumpAndSetEnv(cmd.Context(), sshConf, cmd.Flags(), false))

			var comps []string
			if len(args) == 0 {
				if strings.IndexAny(toComplete, "/.~") == 0 {
					// Looks like a path, do nothing
				} else if strings.Contains(toComplete, ":") {
					// TODO: complete remote files in the pod
				} else if idx := strings.Index(toComplete, "/"); idx > 0 {
					// complete <namespace>/<pod>
					namespace := toComplete[:idx]
					template := "{{ range .items }}{{ .metadata.namespace }}/{{ .metadata.name }}: {{ end }}"
					comps = completion.CompGetFromTemplate(&template, f, namespace, []string{"pod"}, toComplete)
				} else {
					// Complete namespaces followed by a /
					for _, ns := range completion.CompGetResource(f, "namespace", toComplete) {
						comps = append(comps, fmt.Sprintf("%s/", ns))
					}
					// Complete pod names followed by a :
					for _, pod := range completion.CompGetResource(f, "pod", toComplete) {
						comps = append(comps, fmt.Sprintf("%s:", pod))
					}

					// Finally, provide file completion if we need to.
					// We only do this if:
					// 1- There are other completions found (if there are no completions,
					//    the shell will do file completion itself)
					// 2- If there is some input from the user (or else we will end up
					//    listing the entire content of the current directory which could
					//    be too many choices for the user)
					if len(comps) > 0 && len(toComplete) > 0 {
						if files, err := os.ReadDir("."); err == nil {
							for _, file := range files {
								filename := file.Name()
								if strings.HasPrefix(filename, toComplete) {
									if file.IsDir() {
										filename = fmt.Sprintf("%s/", filename)
									}
									// We are completing a file prefix
									comps = append(comps, filename)
								}
							}
						}
					} else if len(toComplete) == 0 {
						// If the user didn't provide any input to complete,
						// we provide a hint that a path can also be used
						comps = append(comps, "./", "/")
					}
				}
			}
			return comps, cobra.ShellCompDirectiveNoSpace
		},
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.Run())
		},
	}
	cmdutil.AddContainerVarFlags(cmd, &o.Container, o.Container)
	cmd.Flags().BoolVarP(&o.NoPreserve, "no-preserve", "", false, "The copied file/directory's ownership and permissions will not be preserved in the container")
	cmd.Flags().IntVarP(&o.MaxTries, "retries", "", 0, "Set number of retries to complete a copy operation from a container. Specify 0 to disable or any negative value for infinite retrying. The default is 0 (no retry).")

	pkgssh.AddSshFlags(cmd.Flags(), sshConf)
	return cmd
}
