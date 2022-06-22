package dns

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	miekgdns "github.com/miekg/dns"

	"github.com/wencaiwulue/kubevpn/util"
)

func TestSetupDnsServer(t *testing.T) {
	port := util.GetAvailableUDPPortOrDie()
	fmt.Println(port)
	go func() {
		err := NewDNSServer("udp", "127.0.0.1:"+strconv.Itoa(port), &miekgdns.ClientConfig{
			Servers: []string{""},
			Search:  []string{"test.svc.cluster.local", "svc.cluster.local", "cluster.local"},
			Port:    "53",
			Ndots:   0,
		})
		if err != nil {
			t.FailNow()
		}
	}()
	time.Sleep(time.Second * 5)
}
