package controlplane

import (
	"context"
	"fmt"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

func Main(filename string, port uint, logger *log.Logger) {
	snapshotCache := cache.NewSnapshotCache(false, cache.IDHash{}, logger)
	proc := NewProcessor(snapshotCache, logger)

	go func() {
		ctx := context.Background()
		server := serverv3.NewServer(ctx, snapshotCache, nil)
		RunServer(ctx, server, port)
	}()

	notifyCh := make(chan NotifyMessage, 100)

	notifyCh <- NotifyMessage{
		Operation: Create,
		FilePath:  filename,
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(fmt.Errorf("failed to create file watcher, err: %v", err))
	}
	defer watcher.Close()
	err = watcher.Add(filename)
	if err != nil {
		log.Fatal(fmt.Errorf("failed to add file: %s to wather, err: %v", filename, err))
	}
	go Watch(watcher, filename, notifyCh)

	for {
		select {
		case msg := <-notifyCh:
			proc.ProcessFile(msg)
		}
	}
}
