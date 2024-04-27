package cmds

import (
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestParseFlags(t *testing.T) {
	str := `
Name: test
Needs: test1
Flags:
  - --kubeconfig ~/.kube/vke
  - --namespace test
  - --ssh-username admin
  - --ssh-addr 127.0.0.1:22

---`
	config, err := ParseConfig([]byte(str))
	if err != nil {
		log.Fatal(err)
	}

	sshConf, bytes, ns, err := GetClusterIDByConfig(config)
	if err != nil {
		log.Fatal(err)
	}
	t.Log(ns, string(bytes))
	t.Log(sshConf.Addr, sshConf.User)
}
