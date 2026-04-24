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
	// Register klog flags on the standard flag set so that they can be
	// configured programmatically.  klog v2 defaults -logtostderr to true,
	// which silently ignores -stderrthreshold.  Opt out of that legacy
	// behaviour (kubernetes/klog#432) so users can control the threshold.
	klog.InitFlags(nil)
	flag.Set("legacy_stderr_threshold_behavior", "false") //nolint:errcheck
	flag.Set("stderrthreshold", "INFO")                   //nolint:errcheck

	ctx := ctrl.SetupSignalHandler()
	_ = cmds.NewKubeVPNCommand().ExecuteContext(ctx)
}
