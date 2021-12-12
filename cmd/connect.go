package cmd

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/wencaiwulue/kubevpn/driver"
	"github.com/wencaiwulue/kubevpn/pkg"
	"github.com/wencaiwulue/kubevpn/util"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"os"
	"path/filepath"
)

var connect pkg.ConnectOptions

func init() {
	connectCmd.Flags().StringVar(&connect.KubeconfigPath, "kubeconfig", clientcmd.RecommendedHomeFile, "kubeconfig")
	connectCmd.Flags().StringVarP(&connect.Namespace, "namespace", "n", "", "namespace")
	connectCmd.PersistentFlags().StringArrayVar(&connect.Workloads, "workloads", []string{}, "workloads, like: services/tomcat, deployment/nginx, replicaset/tomcat...")
	connectCmd.Flags().StringVar((*string)(&connect.Mode), "mode", string(pkg.Reverse), "default mode is reverse")
	connectCmd.Flags().BoolVar(&util.Debug, "debug", false, "true/false")
	RootCmd.AddCommand(connectCmd)
}

var connectCmd = &cobra.Command{
	Use:   "connect",
	Short: "connect",
	Long:  `connect`,
	PreRun: func(*cobra.Command, []string) {
		util.InitLogger(util.Debug)
		if util.IsWindows() {
			driver.InstallWireGuardTunDriver()
		}
	},
	Run: func(cmd *cobra.Command, args []string) {
		if err := connect.InitClient(); err != nil {
			log.Fatal(err)
		}
		connect.DoConnect()
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
