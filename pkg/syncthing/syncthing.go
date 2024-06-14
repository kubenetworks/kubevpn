package syncthing

import (
	"context"
	"net"
	"strconv"

	"github.com/syncthing/syncthing/cmd/syncthing/cmdutil"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/db/backend"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"
	"github.com/syncthing/syncthing/lib/tlsutil"
	"github.com/thejerf/suture/v4"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func StartClient(ctx context.Context, localDir, remoteDir string, localDeviceID, remoteDeviceID string) error {
	if err := MakeSureGui(); err != nil {
		return err
	}

	cert, err := tlsutil.NewCertificateInMemory("syncthing", 36500)
	if err != nil {
		return err
	}
	localDeviceID = protocol.NewDeviceID(cert.Certificate[0]).String()

	localID, _ := protocol.DeviceIDFromString(localDeviceID)
	remoteID, _ := protocol.DeviceIDFromString(remoteDeviceID)
	localPort, _ := util.GetAvailableTCPPortOrDie()

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
	var devices []config.DeviceConfiguration
	devices = append(devices, config.DeviceConfiguration{
		DeviceID: localID, Addresses: []string{addr}, Name: "kubevpn client", Untrusted: false, AutoAcceptFolders: true,
	})
	devices = append(devices, config.DeviceConfiguration{
		DeviceID: remoteID, Addresses: []string{net.JoinHostPort("172.16.48.110", "8384")}, Name: "kubevpn server", Untrusted: false, AutoAcceptFolders: true,
	})

	var folder []config.FolderConfiguration
	folder = append(folder, config.FolderConfiguration{
		ID:      localDir,
		Path:    localDir,
		Type:    config.FolderTypeSendReceive,
		Devices: []config.FolderDeviceConfiguration{{DeviceID: localID, IntroducedBy: remoteID}},
	})
	folder = append(folder, config.FolderConfiguration{
		ID:      remoteDir,
		Path:    remoteDir,
		Devices: []config.FolderDeviceConfiguration{{DeviceID: remoteID, IntroducedBy: localID}},
		Type:    config.FolderTypeSendReceive,
	})

	err = cmdutil.SetConfigDataLocationsFromFlags(daemon.GetSyncthingPath(), "", "")
	if err != nil {
		return err
	}
	err = locations.Set(locations.GUIAssets, daemon.GetSyncthingGUIPath())
	if err != nil {
		return err
	}
	cfg := config.Wrap("configs.xml", config.Configuration{
		Devices: devices,
		Folders: folder,
		GUI: config.GUIConfiguration{
			Enabled:    true,
			RawAddress: addr,
		},
		Options: config.OptionsConfiguration{
			CREnabled:            false,
			AutoUpgradeIntervalH: 0,
			UpgradeToPreReleases: false,
			GlobalAnnEnabled:     false,
		},
	}, localID, events.NoopLogger)

	db := backend.OpenMemory()
	app, err := syncthing.New(cfg, db, events.NoopLogger, cert, syncthing.Options{
		NoUpgrade: true,
		Verbose:   true,
	})
	if err != nil {
		return err
	}

	sup := suture.New("test", svcutil.SpecWithDebugLogger(nil))
	sup.Add(cfg)
	sup.ServeBackground(ctx)

	err = app.Start()
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		app.Stop(svcutil.ExitSuccess)
	}()
	go app.Wait()
	return nil
}

func StartServer(ctx context.Context, detach bool, deviceID string) error {
	if err := MakeSureGui(); err != nil {
		return err
	}

	cert, err := tlsutil.NewCertificateInMemory("syncthing", 36500)
	if err != nil {
		return err
	}

	var id protocol.DeviceID
	id, err = protocol.DeviceIDFromString(deviceID)
	if err != nil {
		return err
	}
	err = cmdutil.SetConfigDataLocationsFromFlags(daemon.GetSyncthingPath(), "", "")
	if err != nil {
		return err
	}
	err = locations.Set(locations.GUIAssets, daemon.GetSyncthingGUIPath())
	if err != nil {
		return err
	}
	addr := "0.0.0.0:8384"
	device := config.DeviceConfiguration{
		DeviceID: id, Addresses: []string{addr}, Name: "kubevpn server", AutoAcceptFolders: true, Untrusted: false,
	}
	cfg := config.Wrap("configs.xml", config.Configuration{
		Defaults: config.Defaults{
			Device: device,
		},
		//Devices: []config.DeviceConfiguration{device},
		GUI: config.GUIConfiguration{
			Enabled:    true,
			RawAddress: addr,
		},
		Options: config.OptionsConfiguration{
			CREnabled:            false,
			AutoUpgradeIntervalH: 0,
			UpgradeToPreReleases: false,
			GlobalAnnEnabled:     false,
		},
	}, id, events.NoopLogger)

	db := backend.OpenMemory()
	app, err := syncthing.New(cfg, db, events.NoopLogger, cert, syncthing.Options{
		NoUpgrade: true,
		Verbose:   true,
	})
	if err != nil {
		return err
	}

	sup := suture.New("test", svcutil.SpecWithDebugLogger(nil))
	sup.Add(cfg)
	sup.ServeBackground(ctx)

	err = app.Start()
	if err != nil {
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
	return nil
}
