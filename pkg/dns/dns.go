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
	"time"

	miekgdns "github.com/miekg/dns"
	v12 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	v13 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

type Config struct {
	Config      *miekgdns.ClientConfig
	Ns          []string
	UseLocalDNS bool
	TunName     string
	// lite mode means connect to another cluster
	Lite bool

	Hosts []Entry
}

func (c *Config) AddServiceNameToHosts(ctx context.Context, serviceInterface v13.ServiceInterface, hosts ...Entry) {
	rateLimiter := flowcontrol.NewTokenBucketRateLimiter(0.2, 1)
	defer rateLimiter.Stop()
	var last string

	serviceList, err := serviceInterface.List(ctx, v1.ListOptions{})
	if err == nil && len(serviceList.Items) != 0 {
		entry := c.generateHostsEntry(serviceList.Items, hosts)
		if entry != "" {
			if err = c.updateHosts(entry); err == nil {
				last = entry
			}
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				func() {
					w, err := serviceInterface.Watch(ctx, v1.ListOptions{
						Watch: true, ResourceVersion: serviceList.ResourceVersion,
					})

					if err != nil {
						if utilnet.IsConnectionRefused(err) || apierrors.IsTooManyRequests(err) {
							time.Sleep(time.Second * 5)
						}
						return
					}
					defer w.Stop()
					for {
						select {
						case event, ok := <-w.ResultChan():
							if !ok {
								return
							}
							if watch.Error == event.Type || watch.Bookmark == event.Type {
								continue
							}
							if !rateLimiter.TryAccept() {
								return
							}
							list, err := serviceInterface.List(ctx, v1.ListOptions{})
							if err != nil {
								return
							}
							entry := c.generateHostsEntry(list.Items, hosts)
							if entry == "" {
								continue
							}
							if entry == last {
								continue
							}
							if err = c.updateHosts(entry); err != nil {
								return
							}
							last = entry
						}
					}
				}()
			}
		}
	}()
}

func (c *Config) updateHosts(str string) error {
	path := GetHostFile()
	file, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(file), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.Contains(line, config.HostsKeyWord) {
			for _, host := range c.Hosts {
				if strings.Contains(line, host.Domain) {
					lines = append(lines[:i], lines[i+1:]...)
					i--
				}
			}
		}
	}
	if len(lines) == 0 {
		return fmt.Errorf("empty hosts file")
	}

	var sb strings.Builder
	sb.WriteString(strings.Join(lines, "\n"))
	if str != "" {
		sb.WriteString("\n")
		sb.WriteString(str)
	}
	s := sb.String()

	// remove last empty line
	s = strings.TrimRight(s, "\n")

	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("empty content after update")
	}

	return os.WriteFile(path, []byte(s), 0644)
}

type Entry struct {
	IP     string
	Domain string
}

func (c *Config) generateHostsEntry(list []v12.Service, hosts []Entry) string {
	const ServiceKubernetes = "kubernetes"
	var entryList []Entry

	// get all service ip
	for _, item := range list {
		if strings.EqualFold(item.Name, ServiceKubernetes) {
			continue
		}
		ipList := sets.New[string](item.Spec.ClusterIPs...).Insert(item.Spec.ExternalIPs...).UnsortedList()
		domainList := sets.New[string](item.Name).Insert(item.Spec.ExternalName).UnsortedList()
		for _, ip := range ipList {
			for _, domain := range domainList {
				if net.ParseIP(ip) == nil || domain == "" {
					continue
				}
				entryList = append(entryList, Entry{IP: ip, Domain: domain})
			}
		}
	}
	entryList = append(entryList, hosts...)
	sort.SliceStable(entryList, func(i, j int) bool {
		if entryList[i].Domain == entryList[j].Domain {
			return entryList[i].IP > entryList[j].IP
		}
		return entryList[i].Domain > entryList[j].Domain
	})

	// if dns already works well, not needs to add it to hosts file
	for i := 0; i < len(entryList); i++ {
		e := entryList[i]
		host, err := net.LookupHost(e.Domain)
		if err == nil && sets.NewString(host...).Has(e.IP) {
			entryList = append(entryList[:i], entryList[i+1:]...)
			i--
		}
	}

	// if hosts file already contains item, not needs to add it to hosts file
	file, err := os.ReadFile(GetHostFile())
	if err != nil {
		return ""
	}
	lines := strings.Split(string(file), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		for j := 0; j < len(entryList); j++ {
			entry := entryList[j]
			if strings.Contains(line, entry.Domain) && strings.Contains(line, entry.IP) {
				entryList = append(entryList[:j], entryList[j+1:]...)
				j--
			}
		}
	}

	c.Hosts = append(c.Hosts, entryList...)
	var sb = new(bytes.Buffer)
	w := tabwriter.NewWriter(sb, 1, 1, 1, ' ', 0)
	for _, e := range entryList {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.IP, e.Domain, "", config.HostsKeyWord)
	}
	_ = w.Flush()
	return sb.String()
}

func CleanupHosts() error {
	path := GetHostFile()
	file, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(file), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.Contains(line, config.HostsKeyWord) {
			lines = append(lines[:i], lines[i+1:]...)
			i--
		}
	}
	if len(lines) == 0 {
		return fmt.Errorf("empty hosts file")
	}

	var sb strings.Builder
	sb.WriteString(strings.Join(lines, "\n"))

	return os.WriteFile(path, []byte(sb.String()), 0644)
}
