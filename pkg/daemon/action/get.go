package action

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/restmapper"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func (svr *Server) Get(ctx context.Context, req *rpc.GetRequest) (*rpc.GetResponse, error) {
	if svr.connect == nil {
		return nil, errors.New("not connected")
	}
	if svr.gr == nil {
		restConfig, err := svr.connect.GetFactory().ToRESTConfig()
		if err != nil {
			return nil, err
		}
		config, err := discovery.NewDiscoveryClientForConfig(restConfig)
		if err != nil {
			return nil, err
		}
		svr.gr, err = restmapper.GetAPIGroupResources(config)
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
		for _, resources := range svr.gr {
			for _, apiResources := range resources.VersionedResources {
				for _, resource := range apiResources {
					have := sets.New[string](resource.Kind, resource.Name, resource.SingularName).Insert(resource.ShortNames...).Has(req.Resource)
					if have {
						resourcesFor, err := mapper.RESTMapping(schema.GroupKind{
							Group: resource.Group,
							Kind:  resource.Kind,
						}, resource.Version)
						if err != nil {
							return nil, err
						}
						svr.informer.ForResource(resourcesFor.Resource)
					}
				}
			}
		}
		go svr.informer.Start(svr.connect.Context().Done())
		go svr.informer.WaitForCacheSync(make(chan struct{}))
	}
	informer, err := svr.getInformer(req)
	if err != nil {
		return nil, err
	}
	var result []*rpc.Metadata
	for _, m := range informer.Informer().GetIndexer().List() {
		object, err := meta.Accessor(m)
		if err != nil {
			return nil, err
		}
		result = append(result, &rpc.Metadata{
			Name:      object.GetName(),
			Namespace: object.GetNamespace(),
		})
	}

	return &rpc.GetResponse{Metadata: result}, nil
}

func (svr *Server) getInformer(req *rpc.GetRequest) (informers.GenericInformer, error) {
	mapper, err := svr.connect.GetFactory().ToRESTMapper()
	if err != nil {
		return nil, err
	}
	var resourcesFor *meta.RESTMapping
out:
	for _, resources := range svr.gr {
		for _, apiResources := range resources.VersionedResources {
			for _, resource := range apiResources {
				have := sets.New[string](resource.Kind, resource.Name, resource.SingularName).Insert(resource.ShortNames...).Has(req.Resource)
				if have {
					resourcesFor, err = mapper.RESTMapping(schema.GroupKind{
						Group: resource.Group,
						Kind:  resource.Kind,
					}, resource.Version)
					if err != nil {
						return nil, err
					}
					break out
				}
			}
		}
	}
	if resourcesFor == nil {
		return nil, errors.New("ErrResourceNotFound")
	}

	return svr.informer.ForResource(resourcesFor.Resource), nil
}
