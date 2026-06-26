package handler

import (
	"context"
	"encoding/json"
	"net"
	"time"

	miekgdns "github.com/miekg/dns"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (c *ConnectOptions) setupDNS(ctx context.Context, svcInformer cache.SharedIndexInformer) error {
	podList, err := c.GetRunningPodList(ctx)
	if err != nil {
		plog.G(ctx).Errorf("Get running pod list failed, err: %v", err)
		return err
	}
	pod := podList[0]
	plog.G(ctx).Infof("Get DNS service IP from Pod...")
	relovConf, err := util.GetDNSServiceIPFromPod(ctx, c.clientset, c.config, pod.GetName(), c.ManagerNamespace)
	if err != nil {
		plog.G(ctx).Errorln(err)
		return err
	}

	marshal, _ := json.Marshal(relovConf)
	plog.G(ctx).Debugf("Get DNS service config: %v", string(marshal))
	var svc *v1.Service
	svc, err = c.clientset.CoreV1().Services(c.ManagerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}
	err = detectNameserver(ctx, relovConf, svc.Spec.ClusterIP, pod.Status.PodIP)
	if err != nil {
		return err
	}

	plog.G(ctx).Infof("Adding extra domain to hosts...")
	if err = c.addExtraRoute(c.ctx, pod.GetName()); err != nil {
		plog.G(ctx).Errorf("Add extra route failed: %v", err)
		return err
	}

	ns := []string{c.OriginNamespace}
	list, err := c.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 500})
	if err == nil {
		for _, item := range list.Items {
			if !sets.New[string](ns...).Has(item.Name) {
				ns = append(ns, item.Name)
			}
		}
	}

	plog.G(ctx).Infof("Listing namespace %s services...", c.OriginNamespace)
	c.dnsConfig = &dns.Config{
		Config:      relovConf,
		Ns:          ns,
		Services:    []v1.Service{},
		SvcInformer: svcInformer,
		TunName:     c.tunName,
		Hosts:       c.extraHost,
		Lock:        c.Lock,
		HowToGetExternalName: func(domain string) (string, error) {
			podList, err := c.GetRunningPodList(ctx)
			if err != nil {
				return "", err
			}
			pod := podList[0]
			return util.Shell(
				ctx,
				c.clientset,
				c.config,
				pod.GetName(),
				config.ContainerSidecarVPN,
				c.ManagerNamespace,
				[]string{"dig", "+short", domain},
			)
		},
	}
	plog.G(ctx).Infof("Setup DNS server for device %s...", c.tunName)
	if err = c.dnsConfig.SetupDNS(ctx); err != nil {
		return err
	}
	plog.G(ctx).Infof("Dump service in namespace %s into hosts...", c.OriginNamespace)
	// dump service in current namespace for support DNS resolve service:port
	err = c.dnsConfig.AddServiceNameToHosts(ctx, c.extraHost...)
	return err
}

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
