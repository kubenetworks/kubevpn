package handler

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func Complete(route *core.Route) error {
	if v, ok := os.LookupEnv(config.EnvInboundPodTunIPv4); ok && v == "" {
		namespace := os.Getenv(config.EnvPodNamespace)
		if namespace == "" {
			return fmt.Errorf("can not get namespace")
		}
		url := fmt.Sprintf("https://%s:80%s", util.GetTlsDomain(namespace), config.APIRentIP)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return fmt.Errorf("can not new req, err: %v", err)
		}
		req.Header.Set(config.HeaderPodName, os.Getenv(config.EnvPodName))
		req.Header.Set(config.HeaderPodNamespace, namespace)
		var ip []byte
		ip, err = util.DoReq(req)
		if err != nil {
			log.Error(err)
			return err
		}
		log.Infof("rent an ip %s", strings.TrimSpace(string(ip)))
		ips := strings.Split(string(ip), ",")
		if len(ips) != 2 {
			return fmt.Errorf("can not get ip from %s", string(ip))
		}
		if err = os.Setenv(config.EnvInboundPodTunIPv4, ips[0]); err != nil {
			log.Error(err)
			return err
		}
		if err = os.Setenv(config.EnvInboundPodTunIPv6, ips[1]); err != nil {
			log.Error(err)
			return err
		}
		for i := 0; i < len(route.ServeNodes); i++ {
			node, err := core.ParseNode(route.ServeNodes[i])
			if err != nil {
				return err
			}
			if node.Protocol == "tun" {
				if get := node.Get("net"); get == "" {
					route.ServeNodes[i] = route.ServeNodes[i] + "&net=" + string(ip)
				}
			}
		}
	}
	return nil
}

func Final() error {
	namespace := os.Getenv(config.EnvPodNamespace)
	url := fmt.Sprintf("https://%s:80%s", util.GetTlsDomain(namespace), config.APIReleaseIP)
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("can not new req, err: %v", err)
	}
	req.Header.Set(config.HeaderPodName, os.Getenv(config.EnvPodName))
	req.Header.Set(config.HeaderPodNamespace, namespace)
	req.Header.Set(config.HeaderIPv4, os.Getenv(config.EnvInboundPodTunIPv4))
	req.Header.Set(config.HeaderIPv6, os.Getenv(config.EnvInboundPodTunIPv6))
	_, err = util.DoReq(req)
	return err
}
