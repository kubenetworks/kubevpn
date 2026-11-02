package xds

import (
	"context"
	"errors"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/xds/internal/discovery"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func routeResponse(frame *discovery.Frame) *rpc.NamespaceRoutesResponse {
	services := make([]*rpc.ServiceRecord, 0, len(frame.UpsertedServices))
	for _, service := range frame.UpsertedServices {
		services = append(services, &rpc.ServiceRecord{Name: service.Name, Namespace: service.Namespace, ClusterIPs: service.ClusterIPs, ExternalName: service.ExternalName})
	}
	return &rpc.NamespaceRoutesResponse{Snapshot: frame.Snapshot, Enabled: frame.Enabled, Version: frame.Version, AddedPodCIDRs: frame.AddedPodCIDRs, RemovedPodCIDRs: frame.RemovedPodCIDRs, UpsertedServices: services, RemovedServiceKeys: frame.RemovedServiceKeys}
}

// WatchNamespaceRoutes streams pod route prefixes and service records for a namespace.
func (s *TunConfigServer) WatchNamespaceRoutes(req *rpc.NamespaceRoutesRequest, stream rpc.TunConfigService_WatchNamespaceRoutesServer) error {
	ch, unsub, err := s.routes.Subscribe(stream.Context(), req.GetNamespace())
	if err != nil {
		if apierrors.IsForbidden(err) {
			return stream.Send(&rpc.NamespaceRoutesResponse{Snapshot: true, Enabled: false})
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return status.FromContextError(err).Err()
		}
		return status.Error(codes.Unavailable, err.Error())
	}
	defer unsub()
	for {
		select {
		case resp := <-ch:
			if err := stream.Send(routeResponse(resp)); err != nil {
				return err
			}
		case <-s.routes.Done():
			return status.FromContextError(s.routes.Err()).Err()
		case <-stream.Context().Done():
			return nil
		}
	}
}
