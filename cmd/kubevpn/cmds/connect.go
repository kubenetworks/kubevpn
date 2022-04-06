package cmds

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	config2 "github.com/wencaiwulue/kubevpn/config"
	"github.com/wencaiwulue/kubevpn/driver"
	"github.com/wencaiwulue/kubevpn/pkg"
	"github.com/wencaiwulue/kubevpn/util"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
)

var connect = pkg.ConnectOptions{}

func init() {
	connectCmd.Flags().StringVar(&connect.KubeconfigPath, "kubeconfig", clientcmd.RecommendedHomeFile, "kubeconfig")
	connectCmd.Flags().StringVarP(&connect.Namespace, "namespace", "n", "", "namespace")
	connectCmd.PersistentFlags().StringArrayVar(&connect.Workloads, "workloads", []string{}, "workloads, like: pods/tomcat, deployment/nginx, replicaset/tomcat...")
	connectCmd.Flags().StringVar((*string)(&connect.Mode), "mode", string(pkg.Reverse), "default mode is reverse")
	connectCmd.Flags().StringToStringVarP(&connect.Headers, "headers", "H", map[string]string{}, "headers, format is k=v, like: k1=v1,k2=v2")
	connectCmd.Flags().BoolVar(&config2.Debug, "debug", false, "true/false")
	RootCmd.AddCommand(connectCmd)
}

var connectCmd = &cobra.Command{
	Use:   "connect",
	Short: "connect",
	Long:  `connect`,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		if !util.IsAdmin() {
			util.RunWithElevated()
			os.Exit(0)
		} else {
			go func() { log.Info(http.ListenAndServe("localhost:6060", nil)) }()
		}
	},
	PreRun: func(*cobra.Command, []string) {
		util.InitLogger(config2.Debug)
		if util.IsWindows() {
			driver.InstallWireGuardTunDriver()
		}
	},
	Run: func(cmd *cobra.Command, args []string) {
		if err := connect.InitClient(); err != nil {
			log.Fatal(err)
		}
		connect.PreCheckResource()
		if err := connect.DoConnect(); err != nil {
			log.Errorln(err)
			pkg.Cleanup(syscall.SIGQUIT)
			return
		}
		fmt.Println(`
-----------------------------------------------------------------------------
  Now you can access resources in the kubernetes cluster, enjoy it
-----------------------------------------------------------------------------`)
		select {}
	},
	PostRun: func(_ *cobra.Command, _ []string) {
		if util.IsWindows() {
			if err := retry.OnError(retry.DefaultRetry, func(err error) bool {
				return err != nil
			}, func() error {
				return driver.UninstallWireGuardTunDriver()
			}); err != nil {
				wd, _ := os.Getwd()
				filename := filepath.Join(wd, "wintun.dll")
				if err = os.Rename(filename, filepath.Join(os.TempDir(), "wintun.dll")); err != nil {
					log.Warn(err)
				}
			}
		}
	},
}
