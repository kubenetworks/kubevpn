package dns

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	miekgdns "github.com/miekg/dns"
	"github.com/pkg/errors"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	v13 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func GetDNSServiceIPFromPod(clientset *kubernetes.Clientset, restclient *rest.RESTClient, config *rest.Config, podName, namespace string) (*miekgdns.ClientConfig, error) {
	var ipp []string
	if ips, err := getDNSIPFromDnsPod(clientset); err == nil {
		ipp = ips
	}
	resolvConfStr, err := util.Shell(clientset, restclient, config, podName, "", namespace, []string{"cat", "/etc/resolv.conf"})
	if err != nil {
		return nil, err
	}
	resolvConf, err := miekgdns.ClientConfigFromReader(bytes.NewBufferString(resolvConfStr))
	if err != nil {
		return nil, err
	}
	if len(ipp) != 0 {
		resolvConf.Servers = append(resolvConf.Servers, make([]string, len(ipp))...)
		copy(resolvConf.Servers[len(ipp):], resolvConf.Servers[:len(resolvConf.Servers)-len(ipp)])
		for i := range ipp {
			resolvConf.Servers[i] = ipp[i]
		}
	}

	// duplicate server
	set := sets.NewString()
	for i := 0; i < len(resolvConf.Servers); i++ {
		if set.Has(resolvConf.Servers[i]) {
			resolvConf.Servers = append(resolvConf.Servers[:i], resolvConf.Servers[i+1:]...)
			i--
		}
		set.Insert(resolvConf.Servers[i])
	}
	return resolvConf, nil
}

func getDNSIPFromDnsPod(clientset *kubernetes.Clientset) (ips []string, err error) {
	var serviceList *v12.ServiceList
	serviceList, err = clientset.CoreV1().Services(v1.NamespaceSystem).List(context.Background(), v1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("k8s-app", "kube-dns").String(),
	})
	if err != nil {
		return nil, err
	}
	for _, item := range serviceList.Items {
		if len(item.Spec.ClusterIP) != 0 {
			ips = append(ips, item.Spec.ClusterIP)
		}
	}
	var podList *v12.PodList
	podList, err = clientset.CoreV1().Pods(v1.NamespaceSystem).List(context.Background(), v1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("k8s-app", "kube-dns").String(),
	})
	if err != nil {
		return
	}
	for _, pod := range podList.Items {
		if pod.Status.Phase == v12.PodRunning && pod.DeletionTimestamp == nil {
			ips = append(ips, pod.Status.PodIP)
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("")
	}
	return ips, nil
}

func AddServiceNameToHosts(ctx context.Context, serviceInterface v13.ServiceInterface) {
	var last string
	for {
		select {
		case <-ctx.Done():
			return
		default:
			func() {
				w, err := serviceInterface.Watch(ctx, v1.ListOptions{})
				if err != nil {
					return
				}
				defer w.Stop()
				for {
					select {
					case c, ok := <-w.ResultChan():
						if !ok {
							return
						}
						if watch.Deleted == c.Type || watch.Error == c.Type {
							continue
						}
						list, err := serviceInterface.List(ctx, v1.ListOptions{})
						if err != nil {
							return
						}
						entry := generateHostsEntry(list.Items)
						if entry == last {
							continue
						}
						err = updateHosts(entry)
						if err != nil {
							return
						}
						last = entry
					}
				}
			}()
		}
	}
}

func updateHosts(str string) error {
	path := GetHostFile()

	file, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	split := strings.Split(string(file), "\n")
	for i := 0; i < len(split); i++ {
		if strings.Contains(split[i], "KubeVPN") {
			split = append(split[:i], split[i+1:]...)
			i--
		}
	}
	var sb strings.Builder

	sb.WriteString(strings.Join(split, "\n"))
	if str != "" {
		sb.WriteString("\n")
		sb.WriteString(str)
	}

	s := sb.String()

	// remove last empty line
	strList := strings.Split(s, "\n")
	for {
		if len(strList) > 0 {
			if strList[len(strList)-1] == "" {
				strList = strList[:len(strList)-1]
				continue
			}
		}
		break
	}

	return os.WriteFile(path, []byte(strings.Join(strList, "\n")), 0644)
}

func generateHostsEntry(list []v12.Service) string {
	type entry struct {
		IP     string
		Domain string
	}

	const ServiceKubernetes = "kubernetes"

	var entryList []entry

	for _, item := range list {
		if strings.EqualFold(item.Name, ServiceKubernetes) {
			continue
		}
		ipList := sets.NewString(item.Spec.ClusterIPs...).Insert(item.Spec.ExternalIPs...).List()
		domainList := sets.NewString(item.Name).Insert(item.Spec.ExternalName).List()
		for _, ip := range ipList {
			for _, domain := range domainList {
				if net.ParseIP(ip) == nil || domain == "" {
					continue
				}
				entryList = append(entryList, entry{IP: ip, Domain: domain})
			}
		}
	}
	sort.SliceStable(entryList, func(i, j int) bool {
		if entryList[i].Domain == entryList[j].Domain {
			return entryList[i].IP > entryList[j].IP
		}
		return entryList[i].Domain > entryList[j].Domain
	})

	var sb = new(bytes.Buffer)
	w := tabwriter.NewWriter(sb, 1, 1, 1, ' ', 0)
	for _, e := range entryList {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.IP, e.Domain, "", "# Add by KubeVPN")
	}
	_ = w.Flush()
	return sb.String()
}
