package syncthing

import (
	"context"
	"net"
	"path/filepath"
	"strconv"

	"github.com/syncthing/syncthing/cmd/syncthing/cmdutil"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/db/backend"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/logger"
	"github.com/syncthing/syncthing/lib/netutil"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"
	"github.com/thejerf/suture/v4"

	pkgconfig "github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

const (
	dir     = "dir"
	local   = "local"
	remote  = "remote"
	label   = "kubevpn"
	guiPort = 8384
)

var (
	conf = filepath.Join(pkgconfig.GetSyncthingPath(), "config.xml")
)

func StartClient(ctx context.Context, localDir string, localAddr, remoteAddr string) error {
	if err := MakeSureGui(); err != nil {
		return err
	}
	err := cmdutil.SetConfigDataLocationsFromFlags(pkgconfig.GetSyncthingPath(), "", "")
	if err != nil {
		return err
	}
	err = locations.Set(locations.GUIAssets, pkgconfig.GetSyncthingGUIPath())
	if err != nil {
		return err
	}
	var l = logger.New().NewFacility("main", "Main package")
	spec := svcutil.SpecWithDebugLogger(l)
	earlyService := suture.New("early", spec)
	earlyService.ServeBackground(ctx)

	evLogger := events.NewLogger()
	earlyService.Add(evLogger)

	var localID = pkgconfig.LocalDeviceID
	var remoteID = pkgconfig.RemoteDeviceID
	var devices []config.DeviceConfiguration
	devices = append(devices, config.DeviceConfiguration{
		DeviceID:          localID,
		Compression:       protocol.CompressionAlways,
		Addresses:         []string{localAddr},
		Name:              local,
		Untrusted:         false,
		AutoAcceptFolders: true,
		Paused:            false,
	})
	devices = append(devices, config.DeviceConfiguration{
		DeviceID:          remoteID,
		Compression:       protocol.CompressionAlways,
		Addresses:         []string{remoteAddr},
		Name:              remote,
		Untrusted:         false,
		AutoAcceptFolders: true,
		Paused:            false,
	})

	var folder []config.FolderConfiguration
	folder = append(folder, config.FolderConfiguration{
		ID:             dir,
		Label:          label,
		FilesystemType: fs.FilesystemTypeBasic,
		Path:           localDir,
		Type:           config.FolderTypeSendReceive,
		Paused:         false,
		Devices: []config.FolderDeviceConfiguration{
			{DeviceID: localID},
			{DeviceID: remoteID},
		},
	})
	cfgWrapper := config.Wrap(conf, config.Configuration{
		Devices: devices,
		Folders: folder,
		GUI: config.GUIConfiguration{
			Enabled:    true,
			RawAddress: localAddr,
		},
		Options: config.OptionsConfiguration{
			AutoUpgradeIntervalH: 0,
			UpgradeToPreReleases: false,
			CREnabled:            false,
		},
	}, localID, events.NoopLogger)
	earlyService.Add(cfgWrapper)

	ldb := backend.OpenMemory()
	appOpts := syncthing.Options{
		NoUpgrade:            true,
		ProfilerAddr:         "",
		ResetDeltaIdxs:       false,
		Verbose:              false,
		DBRecheckInterval:    0,
		DBIndirectGCInterval: 0,
	}

	app, err := syncthing.New(cfgWrapper, ldb, evLogger, pkgconfig.LocalCert, appOpts)
	if err != nil {
		return err
	}

	if err = app.Start(); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		app.Stop(svcutil.ExitSuccess)
	}()
	go app.Wait()
	return app.Error()
}

func StartServer(ctx context.Context, detach bool, remoteDir string) error {
	if err := MakeSureGui(); err != nil {
		return err
	}

	err := cmdutil.SetConfigDataLocationsFromFlags(pkgconfig.GetSyncthingPath(), "", "")
	if err != nil {
		return err
	}
	err = locations.Set(locations.GUIAssets, pkgconfig.GetSyncthingGUIPath())
	if err != nil {
		return err
	}
	spec := svcutil.SpecWithDebugLogger(logger.New().NewFacility("", ""))
	earlyService := suture.New("early", spec)
	earlyService.ServeBackground(ctx)

	evLogger := events.NewLogger()
	earlyService.Add(evLogger)

	var localID = pkgconfig.LocalDeviceID
	var remoteID = pkgconfig.RemoteDeviceID
	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(guiPort))
	var devices []config.DeviceConfiguration
	devices = append(devices, config.DeviceConfiguration{
		DeviceID:          remoteID,
		Compression:       protocol.CompressionAlways,
		Addresses:         []string{addr},
		Name:              remote,
		Untrusted:         false,
		AutoAcceptFolders: true,
		Paused:            false,
	})
	devices = append(devices, config.DeviceConfiguration{
		DeviceID:          localID,
		Compression:       protocol.CompressionAlways,
		Addresses:         []string{"dynamic"},
		Name:              local,
		Untrusted:         false,
		AutoAcceptFolders: true,
		Paused:            false,
	})

	var folder []config.FolderConfiguration
	folder = append(folder, config.FolderConfiguration{
		ID:             dir,
		Label:          label,
		FilesystemType: fs.FilesystemTypeBasic,
		Path:           remoteDir,
		Type:           config.FolderTypeSendReceive,
		Paused:         false,
		Devices: []config.FolderDeviceConfiguration{
			{DeviceID: remoteID},
			{DeviceID: localID},
		},
	})
	cfgWrapper := config.Wrap(conf, config.Configuration{
		Devices: devices,
		Folders: folder,
		GUI: config.GUIConfiguration{
			Enabled:    true,
			RawAddress: addr,
		},
		Options: config.OptionsConfiguration{
			RawListenAddresses:   []string{netutil.AddressURL("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(config.DefaultTCPPort)))},
			CRURL:                "",
			CREnabled:            false,
			AutoUpgradeIntervalH: 0,
			UpgradeToPreReleases: false,
			GlobalAnnEnabled:     false,
		},
	}, remoteID, events.NoopLogger)
	earlyService.Add(cfgWrapper)

	ldb := backend.OpenMemory()
	appOpts := syncthing.Options{
		NoUpgrade:            true,
		ProfilerAddr:         "",
		ResetDeltaIdxs:       false,
		Verbose:              false,
		DBRecheckInterval:    0,
		DBIndirectGCInterval: 0,
	}

	app, err := syncthing.New(cfgWrapper, ldb, evLogger, pkgconfig.RemoteCert, appOpts)
	if err != nil {
		return err
	}

	if err = app.Start(); err != nil {
		return err
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
