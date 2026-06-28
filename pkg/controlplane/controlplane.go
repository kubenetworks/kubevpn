package controlplane

import (
	"context"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// applyDebugLevel raises the logger to DebugLevel when --debug is set. The
// control-plane command binds --debug to config.Debug, and the sidecar is
// deployed with --debug, so its control-plane container logs at Debug by
// default (see pkg/handler/traffmgr_resources.go).
func applyDebugLevel(logger *log.Entry) {
	if config.Debug {
		logger.Logger.SetLevel(log.DebugLevel)
	}
}

// Main starts the envoy xDS control plane: gRPC server, ConfigMap watcher, and snapshot processor.
func Main(ctx context.Context, factory cmdutil.Factory, port uint, logger *log.Entry) error {
	applyDebugLevel(logger)
	snapshotCache := cache.NewSnapshotCache(false, cache.IDHash{}, logger)
	proc := newProcessor(snapshotCache, logger)

	namespace, _, _ := factory.ToRawKubeConfigLoader().Namespace()
	restConfig, _ := factory.ToRESTConfig()
	clientset, _ := kubernetes.NewForConfig(restConfig)

	tunConfig, err := NewTunConfigServer(ctx, clientset, namespace)
	if err != nil {
		plog.G(ctx).Warnf("TunConfigServer init failed (non-fatal): %v", err)
		tunConfig = nil
	} else {
		tunConfig.StartLeaseReaper(ctx)
	}

	errChan := make(chan error, 2)

	go func() {
		server := serverv3.NewServer(ctx, snapshotCache, nil)
		errChan <- runServer(ctx, server, tunConfig, port)
	}()

	notifyCh := make(chan NotifyMessage, 100)

	notifyCh <- NotifyMessage{}
	go func() {
		// tunConfig may be nil when its init failed (non-fatal above); only wire
		// the DHCP reconcile callback when it is available to avoid a nil deref.
		if tunConfig != nil {
			errChan <- Watch(ctx, factory, notifyCh, tunConfig.ReconcileDHCP, tunConfig.ReconcileAllocsFromConfigMap)
		} else {
			errChan <- Watch(ctx, factory, notifyCh)
		}
	}()

	for {
		select {
		case msg := <-notifyCh:
			err := proc.ProcessFile(msg)
			if err != nil {
				plog.G(ctx).Errorf("Failed to process file: %v", err)
				return err
			}
		case err := <-errChan:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
