package syncthing

import (
	"context"
	"github.com/syncthing/syncthing/cmd/syncthing/cmdutil"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/db/backend"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"
	"github.com/syncthing/syncthing/lib/tlsutil"
	"github.com/thejerf/suture/v4"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon"
	"os"
	"testing"
)

func TestSync(t *testing.T) {
	cert, err := tlsutil.NewCertificateInMemory("syncthing", 365)
	if err != nil {
		t.Fatal(err)
	}
	id := protocol.NewDeviceID(cert.Certificate[0])

	//path := daemon.GetSyncPath()
	homePath := daemon.GetHomePath()
	err = cmdutil.SetConfigDataLocationsFromFlags(homePath, "", "")
	if err != nil {
		t.Fatal(err)
	}
	//err = locations.Set(locations.GUIAssets, "~/GolandProjects/syncthing/gui/default")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Wrap("", config.Configuration{
		Devices: []config.DeviceConfiguration{
			{DeviceID: id},
			{DeviceID: id},
		},
		GUI: config.GUIConfiguration{
			Enabled:                   true,
			RawAddress:                "127.0.0.1:8384",
			RawUseTLS:                 false,
			InsecureSkipHostCheck:     true,
			InsecureAllowFrameLoading: true,
			InsecureAdminAccess:       true,
		},
		Options: config.OptionsConfiguration{},
	}, protocol.LocalDeviceID, events.NoopLogger)
	defer os.Remove(cfg.ConfigPath())

	db := backend.OpenMemory()
	app, err := syncthing.New(cfg, db, events.NoopLogger, cert, syncthing.Options{})
	if err != nil {
		t.Fatal(err)
	}

	sup := suture.New("test", svcutil.SpecWithDebugLogger(nil))
	sup.Add(cfg)
	ctx, _ := context.WithCancel(context.Background())
	sup.ServeBackground(ctx)

	startErr := app.Start()

	if startErr != nil {
		t.Fatal(startErr)
	}
	go func() {
		app.Wait()
	}()

	if trans, err := db.NewReadTransaction(); err == nil {
		trans.Release()
	} else if !backend.IsClosed(err) {
		t.Error("Expected error due to db being closed, got", err)
	}
	select {}
}
