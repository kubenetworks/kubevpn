package pkg

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/wencaiwulue/kubevpn/driver"
	"github.com/wencaiwulue/kubevpn/util"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

var (
	nodeConfig     route
	kubeconfigpath string
	namespace      string
	workloads      []string
	clientset      *kubernetes.Clientset
	restclient     *rest.RESTClient
	config         *rest.Config
	factory        cmdutil.Factory
)

func init() {
	connectCmd.Flags().StringVar(&kubeconfigpath, "kubeconfig", clientcmd.RecommendedHomeFile, "kubeconfig")
	connectCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace")
	connectCmd.PersistentFlags().StringArrayVar(&workloads, "workloads", []string{}, "workloads, like: services/tomcat, deployment/nginx, replicaset/tomcat...")
	connectCmd.Flags().BoolVar(&util.Debug, "debug", false, "true/false")
	RootCmd.AddCommand(connectCmd)
}

var connectCmd = &cobra.Command{
	Use:   "connect",
	Short: "connect",
	Long:  `connect`,
	Args: func(cmd *cobra.Command, args []string) error {
		return nil
	},
	Run: func(cmd *cobra.Command, args []string) {
		util.SetupLogger()
		InitClient()
		Main()
	},
	PostRun: func(_ *cobra.Command, _ []string) {
		if util.IsWindows() {
			_ = retry.OnError(retry.DefaultRetry, func(err error) bool {
				return err != nil
			}, func() error {
				return driver.UninstallWireGuardTunDriver()
			})
		}
	},
}

func InitClient() {
	log.Infof("kubeconfig path: %s, namespace: %s, serivces: %v", kubeconfigpath, namespace, workloads)
	var err error
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.KubeConfig = &kubeconfigpath
	factory = cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(configFlags))

	if config, err = factory.ToRESTConfig(); err != nil {
		log.Fatal(err)
	}
	if restclient, err = factory.RESTClient(); err != nil {
		log.Fatal(err)
	}
	if clientset, err = factory.KubernetesClientSet(); err != nil {
		log.Fatal(err)
	}
	if len(namespace) == 0 {
		if namespace, _, err = factory.ToRawKubeConfigLoader().Namespace(); err != nil {
			log.Fatal(err)
		}
	}
}
