package handler

import (
	"context"
	"net"
	"time"

	miekgdns "github.com/miekg/dns"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func detectNameserver(ctx context.Context, relovConf *miekgdns.ClientConfig, serviceIP string, podIP string) error {
	domain := config.ConfigMapPodTrafficManager
	err := nameserverChecker(ctx, domain, serviceIP)
	if err != nil {
		relovConf.Servers = []string{podIP}
		plog.G(ctx).Debugf("DNS service use pod IP %s", podIP)
	} else {
		relovConf.Servers = []string{serviceIP}
		plog.G(ctx).Debugf("DNS service use service IP %s", serviceIP)
	}
	return nil
}

func nameserverChecker(ctx context.Context, domain string, dnsServer string) error {
	msg := new(miekgdns.Msg)
	msg.SetQuestion(miekgdns.Fqdn(domain), miekgdns.TypeA)
	client := miekgdns.Client{Net: "udp", Timeout: time.Second * 10}
	_, _, err := client.ExchangeContext(ctx, msg, net.JoinHostPort(dnsServer, "53"))
	return err
}
