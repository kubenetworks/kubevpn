package controlplane

import (
	"context"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	log "github.com/sirupsen/logrus"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func Main(ctx context.Context, factory cmdutil.Factory, port uint, logger *log.Entry) error {
	snapshotCache := cache.NewSnapshotCache(false, cache.IDHash{}, logger)
	proc := NewProcessor(snapshotCache, logger)

	errChan := make(chan error, 2)

	go func() {
		server := serverv3.NewServer(ctx, snapshotCache, nil)
		errChan <- RunServer(ctx, server, port)
	}()

	notifyCh := make(chan NotifyMessage, 100)

	notifyCh <- NotifyMessage{}
	go func() {
		errChan <- Watch(ctx, factory, notifyCh)
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
