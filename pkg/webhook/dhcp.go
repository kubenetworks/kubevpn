package webhook

import (
	"context"
	"fmt"
	"net"
	"net/http"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
)

type dhcpServer struct {
	f         util.Factory
	clientset *kubernetes.Clientset
}

func (d *dhcpServer) rentIP(w http.ResponseWriter, r *http.Request) {
	podName := r.Header.Get(config.HeaderPodName)
	namespace := r.Header.Get(config.HeaderPodNamespace)
	ctx := context.Background()

	log.Infof("handling rent ip request, pod name: %s, ns: %s", podName, namespace)
	cmi := d.clientset.CoreV1().ConfigMaps(namespace)
	dhcp := handler.NewDHCPManager(cmi, namespace)
	v4, v6, err := dhcp.RentIPRandom(ctx)
	if err != nil {
		log.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	// todo patch annotation
	_, err = w.Write([]byte(fmt.Sprintf("%s,%s", v4.String(), v6.String())))
	if err != nil {
		log.Error(err)
	}
}

func (d *dhcpServer) releaseIP(w http.ResponseWriter, r *http.Request) {
	podName := r.Header.Get(config.HeaderPodName)
	namespace := r.Header.Get(config.HeaderPodNamespace)

	var ips []net.IP
	for _, s := range []string{r.Header.Get(config.HeaderIPv4), r.Header.Get(config.HeaderIPv6)} {
		ip, _, err := net.ParseCIDR(s)
		if err != nil {
			log.Errorf("ip is invailed, ip: %s, err: %v", ip.String(), err)
			continue
		}
		ips = append(ips, ip)
	}

	log.Infof("handling release ip request, pod name: %s, ns: %s", podName, namespace)
	cmi := d.clientset.CoreV1().ConfigMaps(namespace)
	dhcp := handler.NewDHCPManager(cmi, namespace)
	if err := dhcp.ReleaseIP(context.Background(), ips...); err != nil {
		log.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}
