package handler

import (
	"fmt"
	"testing"

	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func TestConnectSSH(t *testing.T) {
	cmd := fmt.Sprintf(`hash kubevpn1 || type kubevpn1 || which kubevpn1 || command -v kubevpn1`)
	serverIP, stderr, err := util.RemoteRun(&util.SshConfig{
		ConfigAlias: "ry-dev-agd",
	}, cmd, nil)

	fmt.Println(string(serverIP), string(stderr), err)
}
