package webhook

import (
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
	podName := r.Header.Get("POD_NAME")
	namespace := r.Header.Get("POD_NAMESPACE")

	log.Infof("handling rent ip request, pod name: %s, ns: %s", podName, namespace)
	cmi := d.clientset.CoreV1().ConfigMaps(namespace)
	dhcp := handler.NewDHCPManager(cmi, namespace, &net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask})
	random, err := dhcp.RentIPRandom()
	if err != nil {
		log.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, err = w.Write([]byte(random.String()))
	if err != nil {
		log.Error(err)
	}
}

func (d *dhcpServer) releaseIP(w http.ResponseWriter, r *http.Request) {
	podName := r.Header.Get("POD_NAME")
	namespace := r.Header.Get("POD_NAMESPACE")
	ip := r.Header.Get("IP")

	_, ipNet, err := net.ParseCIDR(ip)
	if err != nil {
		log.Errorf("ip is invailed, ip: %s, err: %v", ip, err)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(fmt.Sprintf("ip is invailed, ip: %s, err: %v", ip, err)))
		return
	}

	log.Infof("handling release ip request, pod name: %s, ns: %s", podName, namespace)
	cmi := d.clientset.CoreV1().ConfigMaps(namespace)
	dhcp := handler.NewDHCPManager(cmi, namespace, &net.IPNet{IP: config.RouterIP, Mask: config.CIDR.Mask})
	err = dhcp.ReleaseIpToDHCP(ipNet)
	if err != nil {
		log.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}
