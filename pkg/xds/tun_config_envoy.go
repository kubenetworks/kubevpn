package xds

import (
	"context"
	"github.com/wencaiwulue/kubevpn/v2/pkg/xds/internal/envoy"
	"net"
)

func (s *TunConfigServer) syncEnvoyRuleIP(ctx context.Context, owner string, v4, v6 *net.IPNet) {
	envoy.New(s.clientset, s.namespace).Sync(ctx, owner, v4, v6)
}
func (s *TunConfigServer) removeEnvoyRulesForOwner(ctx context.Context, owner string) {
	envoy.New(s.clientset, s.namespace).Remove(ctx, owner)
}
