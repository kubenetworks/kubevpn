package main

import (
	"context"
	"flag"
	"github.com/wencaiwulue/kubevpn/pkg/envoy/internal/processor"
	"github.com/wencaiwulue/kubevpn/pkg/envoy/internal/server"
	"github.com/wencaiwulue/kubevpn/pkg/envoy/internal/watcher"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	log "github.com/sirupsen/logrus"
)

var (
	l log.FieldLogger

	watchDirectoryFileName string
	port                   uint = 9002
	nodeID                      = "test-id"
)

func init() {
	l = log.New()
	log.SetLevel(log.DebugLevel)
	// Define the directory to watch for Envoy configuration files
	flag.StringVar(&watchDirectoryFileName, "watchDirectoryFileName", "/etc/envoy/envoy-config.yaml", "full path to directory to watch for files")
}

func main() {
	flag.Parse()

	// Create a cache
	snampshotcache := cache.NewSnapshotCache(false, cache.IDHash{}, l)

	// Create a processor
	proc := processor.NewProcessor(snampshotcache, nodeID, log.WithField("context", "processor"))

	// Create initial snapshot from file
	proc.ProcessFile(watcher.NotifyMessage{
		Operation: watcher.Create,
		FilePath:  watchDirectoryFileName,
	})

	// Notify channel for file system events
	notifyCh := make(chan watcher.NotifyMessage)

	go func() {
		// Watch for file changes
		watcher.Watch(watchDirectoryFileName, notifyCh)
	}()

	go func() {
		// Run the xDS server
		ctx := context.Background()
		srv := serverv3.NewServer(ctx, snampshotcache, nil)
		server.RunServer(ctx, srv, port)
	}()

	for {
		select {
		case msg := <-notifyCh:
			proc.ProcessFile(msg)
		}
	}
}
