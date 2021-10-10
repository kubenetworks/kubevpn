package dns

import (
	log "github.com/sirupsen/logrus"
	"strconv"
	"testing"
)

func TestName(t *testing.T) {
	//port := util.GetAvailableUDPPortOrDie()
	port := 58477
	err := NewDNSServer("udp", "127.0.0.1:"+strconv.Itoa(port), "172.20.135.131:53", "test")
	if err != nil {
		log.Warnln(err)
	}
}
