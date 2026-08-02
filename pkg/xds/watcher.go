package xds

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	informerv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// NotifyMessage carries ConfigMap content updates from the watcher to the processor.
type NotifyMessage struct {
	Content string
}

// OnDHCPChange is called when the DHCP data in the ConfigMap changes.
type OnDHCPChange func(ctx context.Context)

// cmResyncInterval is the polling fallback interval for the ConfigMap watch loop.
const cmResyncInterval = 5 * time.Second

// Watch monitors the traffic-manager ConfigMap for changes and sends updates to notifyCh.
func Watch(ctx context.Context, f cmdutil.Factory, notifyCh chan<- NotifyMessage, onDHCPChange ...OnDHCPChange) error {
	namespace, _, err := f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	restConfig, err := f.ToRESTConfig()
	if err != nil {
		return err
	}
	conf := rest.CopyConfig(restConfig)
	conf.QPS = 1
	conf.Burst = 2
	clientSet, err := kubernetes.NewForConfig(conf)
	if err != nil {
		plog.G(ctx).Errorf("Failed to create clientset: %v", err)
		return err
	}
	cmIndexers := cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}
	cmInformer := informerv1.NewFilteredConfigMapInformer(clientSet, namespace, 0, cmIndexers, func(options *metav1.ListOptions) {
		options.FieldSelector = fields.OneTermEqualSelector("metadata.name", config.ConfigMapPodTrafficManager).String()
	})
	cmTicker := time.NewTicker(cmResyncInterval)
	_, err = cmInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			cmTicker.Reset(time.Nanosecond * 1)
		},
		UpdateFunc: func(oldObj, newObj any) {
			cmTicker.Reset(time.Nanosecond * 1)
		},
		DeleteFunc: func(obj any) {
			cmTicker.Reset(time.Nanosecond * 1)
		},
	})
	if err != nil {
		plog.G(ctx).Errorf("Failed to add configmap event handler: %v", err)
		return err
	}

	go cmInformer.Run(ctx.Done())
	defer cmTicker.Stop()
	for ; ctx.Err() == nil; <-cmTicker.C {
		cmTicker.Reset(cmResyncInterval)
		cmList := cmInformer.GetIndexer().List()
		if len(cmList) == 0 {
			continue
		}
		for _, cm := range cmList {
			configMap, ok := cm.(*v1.ConfigMap)
			if ok {
				if configMap.Data == nil {
					configMap.Data = make(map[string]string)
				}
				notifyCh <- NotifyMessage{Content: configMap.Data[config.KeyEnvoy]}
				for _, fn := range onDHCPChange {
					fn(ctx)
				}
				continue
			}
		}
	}
	return ctx.Err()
}
