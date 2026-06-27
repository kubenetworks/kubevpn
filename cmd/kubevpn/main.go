package main

import (
	"flag"
	_ "net/http/pprof"

	ctrl "sigs.k8s.io/controller-runtime"

	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/klog/v2"

	"github.com/wencaiwulue/kubevpn/v2/cmd/kubevpn/cmds"
)

func main() {
	// klog v2 defaults -logtostderr=true, which silently ignores -stderrthreshold.
	// Opt out of that legacy behaviour so klog respects the threshold and doesn't
	// dump internal K8s client debug logs to CLI stderr.
	klog.InitFlags(nil)
	_ = flag.Set("legacy_stderr_threshold_behavior", "false")
	_ = flag.Set("stderrthreshold", "INFO")

	ctx := ctrl.SetupSignalHandler()
	_ = cmds.NewKubeVPNCommand().ExecuteContext(ctx)
}
