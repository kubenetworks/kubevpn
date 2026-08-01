package main

import (
	"flag"
	_ "net/http/pprof"
	"os"
	"os/signal"

	ctrl "sigs.k8s.io/controller-runtime"

	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/klog/v2"

	"github.com/wencaiwulue/kubevpn/v2/cmd/kubevpn/cmds"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util/exitcode"
)

func main() {
	// klog v2 defaults -logtostderr=true, which silently ignores -stderrthreshold.
	// Opt out of that legacy behaviour so klog respects the threshold and doesn't
	// dump internal K8s client debug logs to CLI stderr.
	klog.InitFlags(nil)
	_ = flag.Set("legacy_stderr_threshold_behavior", "false")
	_ = flag.Set("stderrthreshold", "INFO")

	// Record SIGINT independently of controller-runtime's handler so we can return
	// exit code 130 on Ctrl-C. The command tree swallows codes.Canceled into a nil
	// error (and the cmds package is frozen), so the returned error alone cannot tell
	// us the run was interrupted. Go fans a signal out to every registered channel, so
	// this coexists with SetupSignalHandler (which still cancels ctx and force-exits on
	// a second signal).
	interrupted := make(chan struct{}, 1)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		select {
		case interrupted <- struct{}{}:
		default:
		}
	}()

	ctx := ctrl.SetupSignalHandler()
	err := cmds.NewKubeVPNCommand().ExecuteContext(ctx)

	code := exitcode.FromError(err)
	select {
	case <-interrupted:
		code = exitcode.Interrupted
	default:
	}
	if code != 0 {
		os.Exit(code)
	}
}
