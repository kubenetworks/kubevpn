package action

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	k8syaml "sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func (svr *Server) List(ctx context.Context, req *rpc.ListRequest) (*rpc.ListResponse, error) {
	if svr.connect == nil {
		return nil, fmt.Errorf("not connect to any cluster")
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
	for _, virtual := range v {
		// deployments.apps.ry-server --> deployments.apps/ry-server
		lastIndex := strings.LastIndex(virtual.Uid, ".")
		virtual.Uid = virtual.Uid[:lastIndex] + "/" + virtual.Uid[lastIndex+1:]
	}
	bytes, err := k8syaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &rpc.ListResponse{Message: string(bytes)}, nil
}
