package syncthing

import (
	"context"
	"testing"
)

func TestSync(t *testing.T) {
	err := StartClient(
		context.Background(),
		"/Users/bytedance/GolandProjects/kubevpn",
		"/app/kubevpn",
		"",
		"NV72NE7-OPOUTTL-M3XV5MD-PMTWT6X-GRUE3WF-Z34YYHX-2YAOZTK-GYYDNQN",
	)
	if err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestUnzip(t *testing.T) {

}
