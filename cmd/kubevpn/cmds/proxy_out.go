package cmds

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"

	"github.com/wencaiwulue/kubevpn/v2/pkg/localproxy"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func CmdProxyOut(f cmdutil.Factory) *cobra.Command {
	var socksListen string
	var httpConnectListen string
	var egress bool

	cmd := &cobra.Command{
		Use:   "proxy-out",
		Short: i18n.T("Expose cluster workloads through a local outbound proxy"),
		Long: templates.LongDesc(i18n.T(`
Expose cluster workloads through a local outbound proxy.

This command is designed for nested VPN cases where OS routes are owned by
another VPN client. Instead of depending on cluster CIDR routing, it resolves
cluster service hostnames and forwards TCP traffic through the Kubernetes API.
Use SOCKS5 with socks5h when you want the proxy to resolve cluster DNS names.

With --egress the proxy instead dials targets directly from the host, using the
host's own DNS and network (socks5h host egress). This reaches the public internet
and, when a KubeVPN VPN/TUN is active, cluster addresses too — without any Kubernetes
API lookup or port-forward.
`)),
		Example: templates.Examples(i18n.T(`
		# Start a local SOCKS5 proxy and access a cluster Service through socks5h
		kubevpn proxy-out --listen-socks 127.0.0.1:1080
		curl --proxy socks5h://127.0.0.1:1080 http://productpage.default.svc.cluster.local:9080

		# Start both SOCKS5 and HTTP CONNECT listeners for TCP traffic
		kubevpn proxy-out --listen-socks 127.0.0.1:1080 --listen-http-connect 127.0.0.1:3128

		# Access a Pod IP or Service ClusterIP through the proxy
		curl --proxy socks5h://127.0.0.1:1080 http://172.21.10.49:9080

		# Host-direct egress: reach the internet (and, under an active VPN, the cluster) via the host
		kubevpn proxy-out --egress --listen-socks 127.0.0.1:1080
		curl --proxy socks5h://127.0.0.1:1080 https://www.google.com
		`)),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SetContext(plog.WithLogger(cmd.Context(), plog.NewClientLogger()))

			if err := validateProxyOutListeners(socksListen, httpConnectListen); err != nil {
				return err
			}

			connector, err := buildProxyOutConnector(f, egress)
			if err != nil {
				return err
			}
			server := &localproxy.Server{
				Connector:         connector,
				SOCKSListenAddr:   socksListen,
				HTTPConnectListen: httpConnectListen,
				Stdout:            os.Stdout,
				Stderr:            os.Stderr,
			}

			if ip := localproxy.FirstNonLoopbackIPv4(); ip != "" {
				_, _ = fmt.Fprintf(os.Stdout, "Local host IPv4 detected: %s\n", ip)
			}
			printProxyOutHints(os.Stdout, socksListen, httpConnectListen)
			return server.ListenAndServe(cmd.Context())
		},
	}

	cmd.Flags().StringVar(&socksListen, "listen-socks", "127.0.0.1:1080", "Local SOCKS5 listen address")
	cmd.Flags().StringVar(&httpConnectListen, "listen-http-connect", "", "Local HTTP CONNECT listen address")
	cmd.Flags().BoolVar(&egress, "egress", false, "Dial targets directly from the host (host DNS + network, socks5h egress) instead of resolving cluster Services via the Kubernetes API")
	return cmd
}

// buildProxyOutConnector selects the proxy's Connector: host-direct egress (no cluster
// client needed) or cluster Service/Pod resolution via the Kubernetes API + port-forward.
func buildProxyOutConnector(f cmdutil.Factory, egress bool) (localproxy.Connector, error) {
	if egress {
		return localproxy.NewHostConnector(), nil
	}
	restConfig, err := f.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	clusterAPI, clientset, err := localproxy.NewClusterAPI(restConfig)
	if err != nil {
		return nil, err
	}
	ns, _, err := f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return nil, err
	}
	return &localproxy.ClusterConnector{
		Client:           clusterAPI,
		Forwarder:        localproxy.NewPodDialer(restConfig, clientset),
		RESTConfig:       restConfig,
		DefaultNamespace: ns,
	}, nil
}

func validateProxyOutListeners(socksListen, httpConnectListen string) error {
	if socksListen == "" && httpConnectListen == "" {
		return fmt.Errorf("at least one of --listen-socks or --listen-http-connect must be set")
	}
	return nil
}

func printProxyOutHints(out io.Writer, socksListen, httpConnectListen string) {
	if out == nil {
		return
	}
	if socksListen != "" {
		_, _ = fmt.Fprintf(out, "Proxy hint: export ALL_PROXY=socks5h://%s\n", socksListen)
	}
	if httpConnectListen != "" {
		_, _ = fmt.Fprintf(out, "Proxy hint: export HTTP_PROXY=http://%s\n", httpConnectListen)
		_, _ = fmt.Fprintf(out, "Proxy hint: export HTTPS_PROXY=http://%s\n", httpConnectListen)
	}
	_, _ = fmt.Fprintln(out, "Proxy scope: TCP traffic only. Use socks5h if you want proxy-side cluster DNS resolution.")
}
