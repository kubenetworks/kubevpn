package main

import (
	"context"
	"flag"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/pkg/control_plane"
	"github.com/wencaiwulue/kubevpn/util"
)

var (
	logger                 log.FieldLogger
	watchDirectoryFileName string
	port                   uint = 9002
)

func init() {
	logger = log.New()
	log.SetLevel(log.DebugLevel)
	log.SetReportCaller(true)
	log.SetFormatter(&util.Format{})
	flag.StringVar(&watchDirectoryFileName, "watchDirectoryFileName", "/etc/envoy/envoy-config.yaml", "full path to directory to watch for files")
}

func main() {
	// Create a cache
	snapshotCache := cache.NewSnapshotCache(false, cache.IDHash{}, logger)
	// Create a processor
	proc := control_plane.NewProcessor(snapshotCache, log.WithField("context", "processor"))

	go func() {
		// Run the xDS server
		ctx := context.Background()
		srv := serverv3.NewServer(ctx, snapshotCache, nil)
		control_plane.RunServer(ctx, srv, port)
	}()

	// Notify channel for file system events
	notifyCh := make(chan control_plane.NotifyMessage)

	go func() {
		// Watch for file changes
		control_plane.Watch(watchDirectoryFileName, notifyCh)
	}()

	for {
		select {
		case msg := <-notifyCh:
			log.Infof("path: %s, event: %v", msg.FilePath, msg.Operation)
			proc.ProcessFile(msg)
		}
	}

	//config, err := rest.InClusterConfig()
	//if err != nil {
	//	panic(err)
	//}
	//clientset, err := kubernetes.NewForConfig(config)
	//if err != nil {
	//	panic(err)
	//}
	//namespace, _, err := util.Namespace()
	//if err != nil {
	//	panic(err)
	//}

	//informerFactory := informers.NewSharedInformerFactoryWithOptions(
	//	clientset,
	//	time.Second*5,
	//	informers.WithNamespace(namespace),
	//	informers.WithTweakListOptions(func(options *metav1.ListOptions) {
	//		options.FieldSelector = fields.OneTermEqualSelector(
	//			"metadata.name", config2.PodTrafficManager,
	//		).String()
	//	}),
	//)
	//informer, err := informerFactory.ForResource(v1.SchemeGroupVersion.WithResource("configmaps"))
	//if err != nil {
	//	panic(err)
	//}
	//informer.Informer().AddEventHandler(
	//	k8scache.FilteringResourceEventHandler{
	//		FilterFunc: func(obj interface{}) bool {
	//			if cm, ok := obj.(*v1.ConfigMap); ok {
	//				if _, found := cm.Data[config2.Envoy]; found {
	//					return true
	//				}
	//			}
	//			return false
	//		},
	//		Handler: k8scache.ResourceEventHandlerFuncs{
	//			AddFunc: func(obj interface{}) {
	//				proc.ProcessConfig(obj.(*v1.ConfigMap).Data[config2.Envoy])
	//			},
	//			UpdateFunc: func(_, newObj interface{}) {
	//				proc.ProcessConfig(newObj.(*v1.ConfigMap).Data[config2.Envoy])
	//			},
	//			DeleteFunc: func(obj interface{}) {
	//				proc.ProcessConfig(obj.(*v1.ConfigMap).Data[config2.Envoy])
	//			},
	//		},
	//	},
	//)
	//informerFactory.Start(context.TODO().Done())
	//informerFactory.WaitForCacheSync(context.TODO().Done())
	//<-context.TODO().Done()
}
