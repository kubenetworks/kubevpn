package action

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func (svr *Server) Get(ctx context.Context, req *rpc.GetRequest) (*rpc.GetResponse, error) {
	if svr.connect == nil || svr.connect.Context() == nil {
		return nil, errors.New("not connected")
	}
	if svr.resourceLists == nil {
		restConfig, err := svr.connect.GetFactory().ToRESTConfig()
		if err != nil {
			return nil, err
		}
		restConfig.WarningHandler = rest.NoWarnings{}
		config, err := discovery.NewDiscoveryClientForConfig(restConfig)
		if err != nil {
			return nil, err
		}
		svr.resourceLists, err = discovery.ServerPreferredResources(config)
		if err != nil {
			return nil, err
		}
		forConfig, err := metadata.NewForConfig(restConfig)
		if err != nil {
			return nil, err
		}
		mapper, err := svr.connect.GetFactory().ToRESTMapper()
		if err != nil {
			return nil, err
		}

		svr.informer = metadatainformer.NewSharedInformerFactory(forConfig, time.Second*5)
		for _, resourceList := range svr.resourceLists {
			for _, resource := range resourceList.APIResources {
				var groupVersion schema.GroupVersion
				groupVersion, err = schema.ParseGroupVersion(resourceList.GroupVersion)
				if err != nil {
					continue
				}
				var mapping schema.GroupVersionResource
				mapping, err = mapper.ResourceFor(groupVersion.WithResource(resource.Name))
				if err != nil {
					if meta.IsNoMatchError(err) {
						continue
					}
					return nil, err
				}
				_ = svr.informer.ForResource(mapping).Informer().SetWatchErrorHandler(func(r *cache.Reflector, err error) {
					_, _ = svr.LogFile.Write([]byte(err.Error()))
				})
			}
		}
		svr.informer.Start(svr.connect.Context().Done())
		svr.informer.WaitForCacheSync(ctx.Done())
	}
	informer, gvk, err := svr.getInformer(req)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, m := range informer.Informer().GetStore().List() {
		objectMetadata, ok := m.(*v1.PartialObjectMetadata)
		if ok {
			deepCopy := objectMetadata.DeepCopy()
			deepCopy.SetGroupVersionKind(*gvk)
			deepCopy.ManagedFields = nil
			marshal, err := json.Marshal(deepCopy)
			if err != nil {
				continue
			}
			result = append(result, string(marshal))
		}
	}
	return &rpc.GetResponse{Metadata: result}, nil
}

func (svr *Server) getInformer(req *rpc.GetRequest) (informers.GenericInformer, *schema.GroupVersionKind, error) {
	mapper, err := svr.connect.GetFactory().ToRESTMapper()
	if err != nil {
		return nil, nil, err
	}
	for _, resources := range svr.resourceLists {
		for _, resource := range resources.APIResources {
			have := sets.New[string](resource.Kind, resource.Name, resource.SingularName).Insert(resource.ShortNames...).Has(req.Resource)
			if have {
				var groupVersion schema.GroupVersion
				groupVersion, err = schema.ParseGroupVersion(resources.GroupVersion)
				if err != nil {
					continue
				}
				var mapping schema.GroupVersionResource
				mapping, err = mapper.ResourceFor(groupVersion.WithResource(resource.Name))
				if err != nil {
					if meta.IsNoMatchError(err) {
						continue
					}
					return nil, nil, err
				}
				return svr.informer.ForResource(mapping), ptr.To(groupVersion.WithKind(resource.Kind)), nil
			}
		}
	}
	return nil, nil, errors.New("ErrResourceNotFound")
}
