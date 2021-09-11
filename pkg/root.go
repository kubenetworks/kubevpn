package main

import (
	"github.com/spf13/cobra"
)

var RootCmd = &cobra.Command{
	Use:   "kubevpn",
	Short: "kubevpn",
	Long:  `kubevpn`,
}

func main() {
	_ = RootCmd.Execute()
}
