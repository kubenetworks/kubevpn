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

	clusterworkload "github.com/wencaiwulue/kubevpn/v2/pkg/cluster/workload"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
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

// ProxyInject publishes node-agent workload, explicit Service, and sync-clone
// attachment state through the manager. It never patches workload PodSpecs or
// Service target ports and has no sidecar fallback.
//
// Progress is streamed to the client as Message lines. An unset factory returns
// codes.Unavailable because workload resolution is unavailable.
func (s *TunConfigServer) ProxyInject(req *rpc.InjectRequest, stream rpc.TunConfigService_ProxyInjectServer) error {
	if s.factory == nil {
		return status.Error(codes.Unavailable, "server-side injection unavailable")
	}
	if req.GetLocalTunIPv4() == "" && len(req.GetSyncRoutes()) == 0 {
		return status.Error(codes.InvalidArgument, "local tun IPv4 is empty")
	}
	if len(req.GetWorkloads()) != 0 && len(req.GetSyncRoutes()) != 0 {
		return status.Error(codes.InvalidArgument, "ordinary workloads and sync routes cannot be mixed in one request")
	}
	if req.GetOwnerID() == "" {
		return status.Error(codes.InvalidArgument, "owner ID must not be empty - client session state may be stale; reconnect and retry")
	}

	logger := injectStreamLogger(func(m string) error {
		return stream.Send(&rpc.InjectResponse{Message: m})
	})
	ctx := plog.WithLogger(stream.Context(), logger)

	plog.StepStart(ctx, "Injecting proxy sidecar (server-side)")
	type injectTarget struct {
		workload   string
		syncTarget *controlplane.SyncTarget
	}
	targets := make([]injectTarget, 0, len(req.GetWorkloads())+len(req.GetSyncRoutes()))
	for _, workload := range req.GetWorkloads() {
		targets = append(targets, injectTarget{workload: workload})
	}
	for _, route := range req.GetSyncRoutes() {
		_, targetController, targetNodeID, err := s.resolveWorkloadNodeID(ctx, req.GetNamespace(), route.GetTargetWorkload())
		if err != nil {
			return err
		}
		targetSpec, _, err := clusterworkload.GetPodTemplateSpecPath(targetController.Object.(*unstructured.Unstructured))
		if err != nil {
			return status.Errorf(codes.Internal, "pod template spec of %s: %v", route.GetTargetWorkload(), err)
		}
		selector := labels.SelectorFromSet(targetSpec.Labels).String()
		if selector == "" {
			return status.Errorf(codes.InvalidArgument, "sync target %s has no pod labels", route.GetTargetWorkload())
		}
		targets = append(targets, injectTarget{workload: route.GetWorkload(), syncTarget: &controlplane.SyncTarget{UID: targetNodeID, Selector: selector}})
	}
	for _, target := range targets {
		workload := target.workload
		object, controller, nodeID, err := s.resolveWorkloadNodeID(ctx, req.GetNamespace(), workload)
		if err != nil {
			return err
		}
		plog.G(ctx).Debugf("[inject] %s portMap=%v", workload, req.GetPortMap())
		injector := inject.NewInjector(inject.InjectOptions{
			Clientset:        s.clientset,
			ManagerNamespace: s.namespace,
			NodeID:           nodeID,
			Object:           object,
			Controller:       controller,
			LocalTunIPv4:     req.GetLocalTunIPv4(),
			LocalTunIPv6:     req.GetLocalTunIPv6(),
			Headers:          req.GetHeaders(),
			PortMaps:         req.GetPortMap(),
			OwnerID:          req.GetOwnerID(),
			SyncTarget:       target.syncTarget,
			ServiceName:      util.If(clusterworkload.IsK8sService(object), object.Name, ""),
		})
		if err := injector.Inject(ctx); err != nil {
			return status.Errorf(codes.Internal, "inject sidecar into %s: %v", workload, err)
		}
		plog.G(ctx).Infof("Injected proxy sidecar into workload %q in namespace %q", workload, req.GetNamespace())
	}
	plog.StepDone(ctx, "Injected proxy sidecar into %d workloads", len(targets))

	return stream.Send(&rpc.InjectResponse{})
}

// LeaveInject removes the caller's node-agent rules by OwnerID. It never
// unpatches workloads or restores Service target ports; deleted targets are
// resolved from stored Virtual identities when necessary.
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
		nodeID := s.resolveLeaveNodeID(ctx, req.GetNamespace(), workload, req.GetOwnerID())
		_, found, err := inject.NewVirtualStore(s.clientset.CoreV1().ConfigMaps(s.namespace)).RemoveRule(ctx, req.GetNamespace(), nodeID, req.GetOwnerID())
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return status.Errorf(codes.Internal, "leave workload %s: %v", workload, err)
		}
		if !found {
			plog.G(ctx).Infof("Not found proxy resource %s", workload)
			continue
		}
		plog.G(ctx).Infof("Left workload %q in namespace %q", workload, req.GetNamespace())
	}
	plog.StepDone(ctx, "Removed proxy from %d workloads", len(workloads))
	return stream.Send(&rpc.InjectResponse{})
}

func (s *TunConfigServer) resolveLeaveNodeID(ctx context.Context, namespace, workload, ownerID string) string {
	_, _, nodeID, err := s.resolveWorkloadNodeID(ctx, namespace, workload)
	if err == nil {
		return nodeID
	}
	fallback := util.ConvertWorkloadToUID(workload)
	cm, getErr := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if getErr != nil {
		return fallback
	}
	virtuals, parseErr := parseYaml(cm.Data[config.KeyEnvoy])
	if parseErr != nil {
		return fallback
	}
	for _, virtual := range virtuals {
		if virtual.Namespace != namespace || !leaveAliasMatches(workload, virtual.UID) {
			continue
		}
		for _, rule := range virtual.Rules {
			if rule.OwnerID == ownerID {
				return virtual.UID
			}
		}
	}
	return fallback
}

func leaveAliasMatches(workload, uid string) bool {
	if util.ConvertWorkloadToUID(workload) == uid {
		return true
	}
	resource, name, found := strings.Cut(workload, "/")
	if !found {
		return false
	}
	index := strings.LastIndex(uid, ".")
	if index < 0 || uid[index+1:] != name {
		return false
	}
	canonical := map[string]string{
		"deploy": "deployments.apps", "deployment": "deployments.apps", "deployments": "deployments.apps",
		"sts": "statefulsets.apps", "statefulset": "statefulsets.apps", "statefulsets": "statefulsets.apps",
		"ds": "daemonsets.apps", "daemonset": "daemonsets.apps", "daemonsets": "daemonsets.apps",
		"rs": "replicasets.apps", "replicaset": "replicasets.apps", "replicasets": "replicasets.apps",
		"svc": "services", "service": "services", "services": "services",
		"po": "pods", "pod": "pods", "pods": "pods",
	}
	return canonical[resource] == uid[:index]
}

// resolveWorkloadNodeID resolves a workload string (group/resource/name) to its top
// owner object, the controller resource.Info, and the envoy nodeID
// ("group.resource/name" of the top owner). Shared by ProxyInject and LeaveInject so
// the resolve+nodeID+error-wrap sequence stays in one place. Returns a gRPC
// status.Errorf (codes.Internal) so callers can return it directly.
func (s *TunConfigServer) resolveWorkloadNodeID(ctx context.Context, namespace, workload string) (object, controller *resource.Info, nodeID string, err error) {
	object, controller, err = clusterworkload.GetTopOwnerObject(ctx, s.factory, namespace, workload)
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
