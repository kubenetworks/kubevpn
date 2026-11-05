package workload

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/kubectl/pkg/cmd/rollout"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// RolloutStatus not use kubectl rollout options is this method can use context to cancel.
//
// undoOnFailure controls the failure-path behavior: when true and the rollout
// does not converge, it runs `kubectl rollout undo` to revert to the previous
// revision. That is a deliberate safety net for the INJECT path (if a freshly
// injected sidecar never becomes ready, undo restores the working version — see
// pkg/inject/container.go). It MUST be false on the unpatch/leave/reset paths:
// there the previous revision is the one WITH the sidecar, so undo would re-apply
// the sidecar we just removed and leave the workload dirty.
func RolloutStatus(ctx1 context.Context, f cmdutil.Factory, ns, workloads string, undoOnFailure bool) (err error) {
	plog.G(ctx1).Debugf("Checking rollout status for %s", workloads)
	defer func() {
		if err == nil {
			plog.G(ctx1).Debugf("Rollout successfully for %s", workloads)
			return
		}
		plog.G(ctx1).Errorf("Rollout status for %s failed: %v", workloads, err)
		if !undoOnFailure {
			return
		}
		out := plog.G(ctx1).Logger.Out
		streams := genericiooptions.IOStreams{
			In:     os.Stdin,
			Out:    out,
			ErrOut: out,
		}
		o := rollout.NewRolloutUndoOptions(streams)
		cmd := &cobra.Command{}
		cmdutil.AddDryRunFlag(cmd)
		_ = o.Complete(f, cmd, []string{workloads})
		_ = o.Validate()
		_ = o.RunUndo()
	}()
	client, _ := f.DynamicClient()
	r := f.NewBuilder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		NamespaceParam(ns).DefaultNamespace().
		ResourceTypeOrNameArgs(true, workloads).
		SingleResourceType().
		Latest().
		Do()
	err = r.Err()
	if err != nil {
		return err
	}

	infos, err := r.Infos()
	if err != nil {
		return err
	}
	if len(infos) != 1 {
		return fmt.Errorf("rollout status is only supported on individual resources and resource collections - %d resources were found", len(infos))
	}
	info := infos[0]
	mapping := info.ResourceMapping()

	statusViewer, err := polymorphichelpers.StatusViewerFn(mapping)
	if err != nil {
		return err
	}

	fieldSelector := fields.OneTermEqualSelector("metadata.name", info.Name).String()
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (k8sruntime.Object, error) {
			options.FieldSelector = fieldSelector
			return client.Resource(info.Mapping.Resource).Namespace(info.Namespace).List(ctx1, options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			options.FieldSelector = fieldSelector
			return client.Resource(info.Mapping.Resource).Namespace(info.Namespace).Watch(ctx1, options)
		},
	}

	// if the rollout isn't done yet, keep watching deployment status, but bound
	// the wait so a sidecar that never becomes ready cannot hang forever.
	ctx, cancel := context.WithTimeout(ctx1, config.RolloutStatusTimeout)
	defer cancel()
	_, err = watchtools.UntilWithSync(ctx, lw, &unstructured.Unstructured{}, nil, func(e watch.Event) (bool, error) {
		switch t := e.Type; t {
		case watch.Added, watch.Modified:
			status, done, err := statusViewer.Status(e.Object.(k8sruntime.Unstructured), 0)
			if err != nil {
				return false, err
			}
			// Quit waiting if the rollout is done
			if done {
				return true, nil
			}
			plog.G(ctx).Debug(strings.TrimSpace(status))
			return false, nil

		case watch.Deleted:
			// We need to abort to avoid cases of recreation and not to silently watch the wrong (new) object
			return true, fmt.Errorf("object has been deleted")

		default:
			return true, fmt.Errorf("internal error: unexpected event %#v", e)
		}
	})
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("rollout status for %s did not finish within %s: %w", workloads, config.RolloutStatusTimeout, err)
	}
	return err
}
