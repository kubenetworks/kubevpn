package xds

import (
	"context"
	"fmt"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/cli-runtime/pkg/resource"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// sendWriter adapts a per-line send callback into an io.Writer, so a logrus
// StreamHook can forward message-only progress lines to the gRPC stream.
type sendWriter struct {
	send func(string) error
}

func (w sendWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg != "" {
		_ = w.send(msg)
	}
	return len(p), nil
}

// injectStreamLogger builds a server logger that writes full output to the pod's
// stdout (the manager container log) and streams message-only progress lines to the
// client via sendMsg. Shared by ProxyInject and LeaveInject.
func injectStreamLogger(sendMsg func(string) error) *log.Logger {
	logger := plog.GetLoggerForServer(int32(log.DebugLevel), os.Stdout)
	logger.AddHook(&plog.StreamHook{
		Writer: sendWriter{send: sendMsg},
		Level:  log.InfoLevel,
	})
	return logger
}

// ProxyInject injects envoy sidecars into the requested workloads server-side, run
// by the traffic manager with its own in-cluster ServiceAccount. It mirrors the
// client-side ConnectOptions.CreateRemoteInboundPod loop, but performs every cluster
// mutation (workload get/patch, envoy ConfigMap rule, rollout wait, fargate service
// target-port) here in the pod. The one side effect that cannot move server-side —
// the SSH reverse-tunnel port Mapper for K8s Service workloads, which forwards to the
// developer's LOCAL machine — is not run here; instead the response reports which
// workloads are Services (plus their pod selector) so the client runs its own Mapper.
//
// Progress is streamed to the client as Message lines. If the factory is unset
// (server-side injection unavailable), it returns codes.Unavailable so the client
// falls back to local injection.
func (s *TunConfigServer) ProxyInject(req *rpc.InjectRequest, stream rpc.TunConfigService_ProxyInjectServer) error {
	if s.factory == nil {
		return status.Error(codes.Unavailable, "server-side injection unavailable")
	}
	if req.GetLocalTunIPv4() == "" {
		return status.Error(codes.InvalidArgument, "local tun IPv4 is empty")
	}

	logger := injectStreamLogger(func(m string) error {
		return stream.Send(&rpc.InjectResponse{Message: m})
	})
	ctx := plog.WithLogger(stream.Context(), logger)

	// The manager runs in the manager namespace, so its own Secret holds the TLS cert.
	tlsSecret, err := s.clientset.CoreV1().Secrets(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return status.Errorf(codes.Internal, "get tls secret: %v", err)
	}

	plog.StepStart(ctx, "Injecting proxy sidecar (server-side)")
	var services []*rpc.ServiceWorkload
	for _, workload := range req.GetWorkloads() {
		object, controller, nodeID, err := s.resolveWorkloadNodeID(ctx, req.GetNamespace(), workload)
		if err != nil {
			return err
		}
		templateSpec, _, err := util.GetPodTemplateSpecPath(controller.Object.(*unstructured.Unstructured))
		if err != nil {
			return status.Errorf(codes.Internal, "pod template spec of %s: %v", workload, err)
		}
		// A K8s Service workload needs a client-side port Mapper (SSH reverse tunnels
		// back to the developer's machine). That cannot run in the pod, so report the
		// workload + its pod selector back and let the client start the Mapper.
		if util.IsK8sService(object) {
			services = append(services, &rpc.ServiceWorkload{
				Workload: workload,
				Selector: labels.SelectorFromSet(templateSpec.Labels).String(),
			})
		}

		injector := inject.NewInjector(inject.InjectOptions{
			Factory:          s.factory,
			Clientset:        s.clientset,
			ManagerNamespace: s.namespace,
			NodeID:           nodeID,
			Object:           object,
			Controller:       controller,
			LocalTunIPv4:     req.GetLocalTunIPv4(),
			LocalTunIPv6:     req.GetLocalTunIPv6(),
			Headers:          req.GetHeaders(),
			PortMaps:         req.GetPortMap(),
			Secret:           tlsSecret,
			Image:            req.GetImage(),
			OwnerID:          req.GetOwnerID(),
		})
		if err := injector.Inject(ctx); err != nil {
			return status.Errorf(codes.Internal, "inject sidecar into %s: %v", workload, err)
		}
		plog.G(ctx).Infof("Injected proxy sidecar into workload %q in namespace %q", workload, req.GetNamespace())
	}
	plog.StepDone(ctx, "Injected proxy sidecar into %d workloads", len(req.GetWorkloads()))

	// Final frame: report Service workloads needing a client-side Mapper.
	return stream.Send(&rpc.InjectResponse{Services: services})
}

// LeaveInject removes the caller's envoy rule (by OwnerID) from workloads server-side:
// the traffic manager unpatches the sidecar when the workload has no rules left, waits
// for rollout and (fargate) restores the service target port, using its own in-cluster
// ServiceAccount.
//
// If Workloads is empty it removes the OwnerID's rules from ALL workloads it currently
// proxies (derived from ENVOY_CONFIG) — used by the client's disconnect cleanup, since
// with server-side injection the client no longer tracks which mesh workloads it proxied.
// Only Namespace/Workloads/OwnerID of the request are used.
func (s *TunConfigServer) LeaveInject(req *rpc.InjectRequest, stream rpc.TunConfigService_LeaveInjectServer) error {
	if s.factory == nil {
		return status.Error(codes.Unavailable, "server-side injection unavailable")
	}

	logger := injectStreamLogger(func(m string) error {
		return stream.Send(&rpc.InjectResponse{Message: m})
	})
	ctx := plog.WithLogger(stream.Context(), logger)

	workloads := req.GetWorkloads()
	if len(workloads) == 0 {
		// Leave-all: derive the workloads this owner proxies in this namespace.
		var err error
		if workloads, err = s.workloadsOwnedBy(ctx, req.GetNamespace(), req.GetOwnerID()); err != nil {
			return status.Errorf(codes.Internal, "list owned workloads: %v", err)
		}
	}

	plog.StepStart(ctx, "Removing proxy from workloads (server-side)")
	for _, workload := range workloads {
		object, controller, nodeID, err := s.resolveWorkloadNodeID(ctx, req.GetNamespace(), workload)
		if err != nil {
			return err
		}
		empty, err := inject.UnpatchContainer(ctx, nodeID, s.factory, s.clientset.CoreV1().ConfigMaps(s.namespace), controller, req.GetOwnerID())
		if err != nil {
			return status.Errorf(codes.Internal, "leave workload %s: %v", workload, err)
		}
		// When this owner was the last rule, the sidecar is removed; a fargate Service
		// also needs its target port restored to the original.
		if empty && util.IsK8sService(object) {
			if err := inject.RestoreServiceTargetPort(ctx, s.clientset, req.GetNamespace(), object.Name); err != nil {
				return status.Errorf(codes.Internal, "restore service %s target port: %v", object.Name, err)
			}
		}
		plog.G(ctx).Infof("Left workload %q in namespace %q", workload, req.GetNamespace())
	}
	plog.StepDone(ctx, "Removed proxy from %d workloads", len(workloads))
	return stream.Send(&rpc.InjectResponse{})
}

// resolveWorkloadNodeID resolves a workload string (group/resource/name) to its top
// owner object, the controller resource.Info, and the envoy nodeID
// ("group.resource/name" of the top owner). Shared by ProxyInject and LeaveInject so
// the resolve+nodeID+error-wrap sequence stays in one place. Returns a gRPC
// status.Errorf (codes.Internal) so callers can return it directly.
func (s *TunConfigServer) resolveWorkloadNodeID(ctx context.Context, namespace, workload string) (object, controller *resource.Info, nodeID string, err error) {
	object, controller, err = util.GetTopOwnerObject(ctx, s.factory, namespace, workload)
	if err != nil {
		return nil, nil, "", status.Errorf(codes.Internal, "resolve workload %s: %v", workload, err)
	}
	nodeID = fmt.Sprintf("%s.%s", object.Mapping.Resource.GroupResource().String(), object.Name)
	return object, controller, nodeID, nil
}

// workloadsOwnedBy returns the workloads (as "group.resource/name") in the given
// namespace that currently carry an envoy rule owned by ownerID, read from the
// ENVOY_CONFIG ConfigMap. Used by LeaveInject's leave-all (disconnect) path. The
// namespace filter matters for a central manager where the same OwnerID (stable per
// machine+user) may proxy workloads in several namespaces — leave-all for one namespace
// must not touch another's rules. Returns nil (not an error) when the config is
// absent/empty/unparseable — nothing to clean up.
func (s *TunConfigServer) workloadsOwnedBy(ctx context.Context, namespace, ownerID string) ([]string, error) {
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	virtuals, err := parseYaml(cm.Data[config.KeyEnvoy])
	if err != nil {
		return nil, nil // empty/legacy/corrupt: nothing to clean up
	}
	var workloads []string
	for _, v := range virtuals {
		if v.Namespace != namespace {
			continue
		}
		for _, r := range v.Rules {
			if r.OwnerID == ownerID {
				workloads = append(workloads, util.ConvertUIDToWorkload(v.UID))
				break
			}
		}
	}
	return workloads, nil
}
