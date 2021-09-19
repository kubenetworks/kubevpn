package main

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"kubevpn/gost"
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
	gost.SetLogger(&gost.LogLogger{})
	connectCmd.Flags().StringVar(&kubeconfigpath, "kubeconfig", clientcmd.RecommendedHomeFile, "kubeconfig")
	connectCmd.Flags().StringVarP(&namespace, "namespace", "n", "", "namespace")
	connectCmd.PersistentFlags().StringArrayVar(&workloads, "workloads", []string{}, "workloads, like: services/tomcat, deployment/nginx, replicaset/tomcat...")
	connectCmd.Flags().BoolVar(&gost.Debug, "debug", false, "true/false")
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
		InitClient()
		Main()
		// hang up
		select {}
	},
}

func InitClient() {
	log.Printf("kubeconfig path: %s, namespace: %s, serivces: %v\n", kubeconfigpath, namespace, workloads)
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
