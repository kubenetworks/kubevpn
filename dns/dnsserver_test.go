package dns

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/util"
	"strconv"
	"testing"
)

func TestName(t *testing.T) {
	port := util.GetAvailableUDPPortOrDie()
	fmt.Println(port)
	err := NewDNSServer("udp", "127.0.0.1:"+strconv.Itoa(port), "172.20.135.131:53", "test")
	if err != nil {
		log.Warnln(err)
	}
}
