package controlplane

import (
	"context"
	"fmt"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

func Main(ctx context.Context, filename string, port uint, logger *log.Logger) error {
	snapshotCache := cache.NewSnapshotCache(false, cache.IDHash{}, logger)
	proc := NewProcessor(snapshotCache, logger)

	errChan := make(chan error, 2)

	go func() {
		server := serverv3.NewServer(ctx, snapshotCache, nil)
		errChan <- RunServer(ctx, server, port)
	}()

	notifyCh := make(chan NotifyMessage, 100)

	notifyCh <- NotifyMessage{
		Operation: Create,
		FilePath:  filename,
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create file watcher: %v", err)
	}
	defer watcher.Close()
	err = watcher.Add(filename)
	if err != nil {
		return fmt.Errorf("failed to add file: %s to wather: %v", filename, err)
	}
	go func() {
		errChan <- Watch(watcher, filename, notifyCh)
	}()

	for {
		select {
		case msg := <-notifyCh:
			err = proc.ProcessFile(msg)
			if err != nil {
				log.Errorf("Failed to process file: %v", err)
				return err
			}
		case err = <-errChan:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
