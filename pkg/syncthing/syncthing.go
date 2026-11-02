package syncthing

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"strconv"

	"github.com/syncthing/syncthing/cmd/syncthing/cmdutil"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/db/backend"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/logger"
	"github.com/syncthing/syncthing/lib/netutil"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"
	"github.com/thejerf/suture/v4"

	pkgconfig "github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

const (
	dir            = "dir"
	local          = "local"
	remote         = "remote"
	label          = "kubevpn"
	guiPort        = 8384
	rescanInterval = 3
)

var (
	conf = filepath.Join(pkgconfig.GetSyncthingPath(), "config.xml")
)

// initServices sets config data locations, creates an early service supervisor
// and an event logger, returning both for use by StartClient/StartServer.
func initServices(ctx context.Context, facilityName, facilityDesc string) (*suture.Supervisor, events.Logger, <-chan error, error) {
	if err := cmdutil.SetConfigDataLocationsFromFlags(pkgconfig.GetSyncthingPath(), "", ""); err != nil {
		return nil, nil, nil, fmt.Errorf("set syncthing data location: %w: %w", err, pkgconfig.ErrSyncthing)
	}
	spec := svcutil.SpecWithDebugLogger(logger.New().NewFacility(facilityName, facilityDesc))
	earlyService := suture.New("early", spec)
	done := earlyService.ServeBackground(ctx)

	evLogger := events.NewLogger()
	earlyService.Add(evLogger)
	return earlyService, evLogger, done, nil
}

// newDevice builds a DeviceConfiguration with common defaults.
func newDevice(id protocol.DeviceID, name string, addresses ...string) config.DeviceConfiguration {
	return config.DeviceConfiguration{
		DeviceID:          id,
		Compression:       config.CompressionAlways,
		Addresses:         addresses,
		Name:              name,
		AutoAcceptFolders: true,
	}
}

// startApp creates a syncthing application from the given config, starts it,
// and arranges for graceful shutdown when ctx is cancelled. If detach is true
// the caller does not block on app completion.
func startApp(ctx context.Context, cfgWrapper config.Wrapper, evLogger events.Logger, cert tls.Certificate, detach bool) error {
	app, err := newApp(cfgWrapper, evLogger, cert)
	if err != nil {
		return err
	}
	if err = app.Start(); err != nil {
		return fmt.Errorf("start syncthing app: %w: %w", err, pkgconfig.ErrSyncthing)
	}
	go func() {
		<-ctx.Done()
		app.Stop(svcutil.ExitSuccess)
	}()
	if detach {
		go app.Wait()
	} else {
		app.Wait()
	}
	return app.Error()
}

// ClientHandle owns the application and its early services. Done closes only
// after both have exited; Stop is idempotent and waits for that completion.
type ClientHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func (c *ClientHandle) Stop()                 { c.cancel(); <-c.done }
func (c *ClientHandle) Done() <-chan struct{} { return c.done }
func (c *ClientHandle) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// application is the lifecycle consumed here, also allowing controlled shutdown
// tests without starting listeners or using the user's Syncthing directory.
type application interface {
	Start() error
	Stop(svcutil.ExitStatus) svcutil.ExitStatus
	Wait() svcutil.ExitStatus
	Error() error
}

func newApp(cfg config.Wrapper, evLogger events.Logger, cert tls.Certificate) (application, error) {
	ldb := backend.OpenMemory()
	app, err := syncthing.New(cfg, ldb, evLogger, cert, syncthing.Options{NoUpgrade: true})
	if err != nil {
		_ = ldb.Close()
		return nil, fmt.Errorf("create syncthing app: %w: %w", err, pkgconfig.ErrSyncthing)
	}
	return app, nil
}

func startClientApp(ctx context.Context, cancel context.CancelFunc, earlyDone <-chan error, app application) (*ClientHandle, error) {
	if err := app.Start(); err != nil {
		app.Stop(svcutil.ExitSuccess)
		app.Wait()
		cancel()
		<-earlyDone
		return nil, fmt.Errorf("start syncthing app: %w: %w", err, pkgconfig.ErrSyncthing)
	}
	h := &ClientHandle{cancel: cancel, done: make(chan struct{})}
	appDone := make(chan struct{})
	go func() { app.Wait(); close(appDone) }()
	go func() {
		select {
		case <-ctx.Done():
			app.Stop(svcutil.ExitSuccess)
		case <-appDone:
		}
		<-appDone
		cancel()
		<-earlyDone
		h.err = app.Error()
		close(h.done)
	}()
	return h, nil
}

// StartClient starts a client syncing localDir with the remote peer. The caller
// owns the returned handle and must Stop it before tearing down its session.
func StartClient(ctx context.Context, localDir string, localAddr, remoteAddr string) (*ClientHandle, error) {
	ctx, cancel := context.WithCancel(ctx)
	earlyService, evLogger, earlyDone, err := initServices(ctx, "main", "Main package")
	if err != nil {
		cancel()
		return nil, err
	}

	localID := pkgconfig.LocalDeviceID
	remoteID := pkgconfig.RemoteDeviceID
	devices := []config.DeviceConfiguration{
		newDevice(localID, local, localAddr),
		newDevice(remoteID, remote, remoteAddr),
	}

	folders := []config.FolderConfiguration{{
		ID:               dir,
		Label:            label,
		FilesystemType:   config.FilesystemTypeBasic,
		Path:             localDir,
		Type:             config.FolderTypeSendReceive,
		FSWatcherEnabled: true,
		FSWatcherDelayS:  0.01,
		RescanIntervalS:  rescanInterval,
		Devices: []config.FolderDeviceConfiguration{
			{DeviceID: localID},
			{DeviceID: remoteID},
		},
	}}

	cfgWrapper := config.Wrap(conf, config.Configuration{
		Devices: devices,
		Folders: folders,
		GUI: config.GUIConfiguration{
			Enabled:    true,
			RawAddress: localAddr,
			APIKey:     pkgconfig.SyncthingAPIKey,
		},
	}, localID, events.NoopLogger)
	earlyService.Add(cfgWrapper)

	app, err := newApp(cfgWrapper, evLogger, pkgconfig.LocalCert)
	if err != nil {
		cancel()
		<-earlyDone
		return nil, err
	}
	return startClientApp(ctx, cancel, earlyDone, app)
}

// StartServer starts a syncthing instance configured as a server, serving remoteDir for sync.
func StartServer(ctx context.Context, detach bool, remoteDir string) error {
	earlyService, evLogger, _, err := initServices(ctx, "", "")
	if err != nil {
		return err
	}

	localID := pkgconfig.LocalDeviceID
	remoteID := pkgconfig.RemoteDeviceID
	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(guiPort))
	devices := []config.DeviceConfiguration{
		newDevice(remoteID, remote, addr),
		newDevice(localID, local, "dynamic"),
	}

	folders := []config.FolderConfiguration{{
		ID:             dir,
		Label:          label,
		FilesystemType: config.FilesystemTypeBasic,
		Path:           remoteDir,
		Type:           config.FolderTypeSendReceive,
		Devices: []config.FolderDeviceConfiguration{
			{DeviceID: remoteID},
			{DeviceID: localID},
		},
	}}

	cfgWrapper := config.Wrap(conf, config.Configuration{
		Devices: devices,
		Folders: folders,
		GUI: config.GUIConfiguration{
			Enabled:    true,
			RawAddress: addr,
		},
		Options: config.OptionsConfiguration{
			RawListenAddresses: []string{netutil.AddressURL("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(config.DefaultTCPPort)))},
			GlobalAnnEnabled:   false,
		},
	}, remoteID, events.NoopLogger)
	earlyService.Add(cfgWrapper)

	return startApp(ctx, cfgWrapper, evLogger, pkgconfig.RemoteCert, detach)
}
