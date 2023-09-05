package action

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func (svr *Server) List(ctx context.Context, req *rpc.ListRequest) (*rpc.ListResponse, error) {
	if svr.connect == nil {
		return nil, fmt.Errorf("no proxy workloads found")
	}
	mapInterface := svr.connect.GetClientset().CoreV1().ConfigMaps(svr.connect.Namespace)
	configMap, err := mapInterface.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var v = make([]*controlplane.Virtual, 0)
	if str, ok := configMap.Data[config.KeyEnvoy]; ok {
		if err = yaml.Unmarshal([]byte(str), &v); err != nil {
			return nil, err
		}
	}
	var workloads []string
	for _, virtual := range v {
		workloads = append(workloads, virtual.Uid)
	}
	return &rpc.ListResponse{
		Workloads: workloads,
	}, nil
}
