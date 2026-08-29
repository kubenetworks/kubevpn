package dns

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"

	miekgdns "github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"tailscale.com/net/dns"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// Config holds DNS configuration for setting up cluster DNS resolution on the local machine.
type Config struct {
	Config   *miekgdns.ClientConfig
	Ns       []string
	Services []corev1.Service
	TunName  string

	Hosts []Entry
	Lock  *sync.Mutex

	// extra is the base set of host entries (extra domains) always applied,
	// preserved so a push-driven UpdateServices can re-fold them in.
	extra []Entry

	HowToGetExternalName func(name string) (string, error)

	// only set it on linux
	OSConfigurator dns.OSConfigurator
}

// AddServiceNameToHosts writes the initial service-name host entries. Unlike the
// former informer-driven design, it does NOT start a watch goroutine: service
// records now arrive server-side via the route watcher, which calls UpdateServices.
// It returns the number of host entries written.
func (c *Config) AddServiceNameToHosts(ctx context.Context, hosts ...Entry) (int, error) {
	c.Lock.Lock()
	c.extra = hosts
	appendHosts := c.generateAppendHosts(c.Services, hosts)
	err := c.appendHosts(appendHosts)
	c.Lock.Unlock()
	if err != nil {
		plog.G(ctx).Errorf("Failed to add hosts(%s): %v", c.entryList2String(appendHosts), err)
		return 0, err
	}
	return len(appendHosts), nil
}

// UpdateServices replaces the known service set (push-driven by the client route
// watcher, which receives ServiceRecords from the traffic manager) and appends any
// new service-name host entries. Hosts are add-only, matching the previous
// informer-driven behavior. On macOS it also refreshes the per-service resolver files.
func (c *Config) UpdateServices(ctx context.Context, services []corev1.Service) {
	c.Lock.Lock()
	c.Services = services
	appendHosts := c.generateAppendHosts(services, c.extra)
	err := c.appendHosts(appendHosts)
	c.Lock.Unlock()
	if err != nil && !errors.Is(err, context.Canceled) {
		plog.G(ctx).Errorf("Failed to add hosts(%s) to hosts: %v", c.entryList2String(appendHosts), err)
	}
	c.applyResolvers(ctx)
}

// param: entry list is needs to added
// 1) check whether already exist, if exist not needs to add
// 2) check whether already can find this host, not needs to add
// 3) otherwise add it to hosts file
func (c *Config) appendHosts(appendHosts []Entry) error {
	if len(appendHosts) == 0 {
		return nil
	}

	existing := sets.New[Entry]().Insert(c.Hosts...)
	for _, appendHost := range appendHosts {
		if !existing.Has(appendHost) {
			existing.Insert(appendHost)
			c.Hosts = append(c.Hosts, appendHost)
		}
	}

	str := c.entryList2String(appendHosts)
	return withHostsFileLock(func() error {
		hostFile := getHostFile()
		f, err := os.OpenFile(hostFile, os.O_APPEND|os.O_WRONLY, config.FileModeFile)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString(str)
		return err
	})
}

func (c *Config) removeHosts() error {
	keyword := fmt.Sprintf(config.HostsDeviceKeyword, c.TunName)
	return filterHostsFile(keyword)
}

// Entry represents a single IP-to-domain mapping for the system hosts file.
type Entry struct {
	IP     string
	Domain string
}

func (c *Config) entryList2String(entryList []Entry) string {
	var sb = new(bytes.Buffer)
	w := tabwriter.NewWriter(sb, 1, 1, 1, ' ', 0)
	for _, e := range entryList {
		_, _ = fmt.Fprintf(w, "\n%s\t%s\t%s\t%s", e.IP, e.Domain, "", fmt.Sprintf(config.HostsDeviceKeyword, c.TunName))
	}
	_ = w.Flush()
	return sb.String()
}

func (c *Config) generateAppendHosts(serviceList []corev1.Service, hosts []Entry) []Entry {
	const ServiceKubernetes = "kubernetes"
	// seen tracks membership in O(1); reused across the loop instead of rebuilding a set from the
	// whole entryList per service (which was O(n^2) on large clusters).
	var seen = sets.New[Entry]().Insert(c.Hosts...).Insert(hosts...)
	var entryList = seen.UnsortedList()

	// 1) add only if not exist
	for _, service := range serviceList {
		if strings.EqualFold(service.Name, ServiceKubernetes) {
			continue
		}
		var ip net.IP
		if service.Spec.ClusterIP != "" {
			ip = net.ParseIP(service.Spec.ClusterIP)
		}
		if service.Spec.ExternalName != "" {
			name, _ := c.HowToGetExternalName(service.Spec.ExternalName)
			ip = net.ParseIP(name)
		}
		if ip == nil {
			continue
		}

		var e = Entry{IP: ip.String(), Domain: service.Name}
		if !seen.Has(e) {
			seen.Insert(e)
			entryList = append([]Entry{e}, entryList...)
		}
	}

	// 2) if hosts file already contains item, not needs to add it to hosts file
	hostFile := getHostFile()
	content, err2 := os.ReadFile(hostFile)
	if err2 == nil {
		reader := bufio.NewReader(strings.NewReader(string(content)))
		for {
			line, err := reader.ReadString('\n')
			for i := 0; i < len(entryList); i++ {
				if strings.Contains(line, config.HostsKeyword) && strings.Contains(line, entryList[i].Domain) {
					entryList = append(entryList[:i], entryList[i+1:]...)
					i--
				}
			}
			if errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				break
			}
		}
	}

	sort.SliceStable(entryList, func(i, j int) bool {
		return entryList[i].Domain > entryList[j].Domain
	})
	return entryList
}

// CleanupHosts removes all KubeVPN-managed entries from the system hosts file.
func CleanupHosts() error {
	return filterHostsFile(config.HostsKeyword)
}

// filterHostsFile removes all lines containing the given keyword from the hosts file.
func filterHostsFile(keyword string) error {
	return withHostsFileLock(func() error {
		hostFile := getHostFile()
		content, err := os.ReadFile(hostFile)
		if err != nil {
			return err
		}
		if !strings.Contains(string(content), config.HostsKeyword) {
			return nil
		}

		var retain []string
		reader := bufio.NewReader(bytes.NewReader(content))
		for {
			line, readErr := reader.ReadString('\n')
			if !strings.Contains(line, keyword) {
				retain = append(retain, line)
			}
			if errors.Is(readErr, io.EOF) {
				break
			} else if readErr != nil {
				return readErr
			}
		}
		if len(retain) == 0 {
			return fmt.Errorf("hosts file would be empty after filtering")
		}

		var sb strings.Builder
		for _, s := range retain {
			sb.WriteString(s)
		}
		str := strings.TrimSuffix(sb.String(), "\n")
		return os.WriteFile(hostFile, []byte(str), config.FileModeFile)
	})
}
