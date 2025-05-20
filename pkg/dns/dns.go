package dns

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	miekgdns "github.com/miekg/dns"
	"github.com/pkg/errors"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"tailscale.com/net/dns"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type Config struct {
	Config      *miekgdns.ClientConfig
	Ns          []string
	Services    []v12.Service
	SvcInformer cache.SharedIndexInformer
	TunName     string

	Hosts []Entry
	Lock  *sync.Mutex

	HowToGetExternalName func(name string) (string, error)

	// only set it on linux
	OSConfigurator dns.OSConfigurator
}

func (c *Config) AddServiceNameToHosts(ctx context.Context, informer cache.SharedIndexInformer, hosts ...Entry) error {
	var serviceList []v12.Service
	c.Lock.Lock()
	defer c.Lock.Unlock()

	appendHosts := c.generateAppendHosts(serviceList, hosts)
	err := c.appendHosts(appendHosts)
	if err != nil {
		plog.G(ctx).Errorf("Failed to add hosts(%s): %v", entryList2String(appendHosts), err)
		return err
	}

	go c.watchServiceToAddHosts(ctx, informer, hosts)
	return nil
}

func (c *Config) watchServiceToAddHosts(ctx context.Context, informer cache.SharedIndexInformer, hosts []Entry) {
	defer util.HandleCrash()
	ticker := time.NewTicker(time.Second * 15)
	defer ticker.Stop()

	for ; ctx.Err() == nil; <-ticker.C {
		serviceList := informer.GetIndexer().List()
		var services []v12.Service
		for _, service := range serviceList {
			svc, ok := service.(*v12.Service)
			if !ok {
				continue
			}
			services = append(services, *svc)
		}
		if len(services) == 0 {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		c.Lock.Lock()
		appendHosts := c.generateAppendHosts(services, hosts)
		err := c.appendHosts(appendHosts)
		c.Lock.Unlock()
		if err != nil && !errors.Is(err, context.Canceled) {
			plog.G(ctx).Errorf("Failed to add hosts(%s) to hosts: %v", entryList2String(appendHosts), err)
		}
	}

}

// param: entry list is needs to added
// 1) check whether already exist, if exist not needs to add
// 2) check whether already can find this host, not needs to add
// 3) otherwise add it to hosts file
func (c *Config) appendHosts(appendHosts []Entry) error {
	if len(appendHosts) == 0 {
		return nil
	}

	for _, appendHost := range appendHosts {
		if !sets.New[Entry]().Insert(c.Hosts...).Has(appendHost) {
			c.Hosts = append(c.Hosts, appendHost)
		}
	}

	hostFile := GetHostFile()
	f, err := os.OpenFile(hostFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	str := entryList2String(appendHosts)
	_, err = f.WriteString(str)
	return err
}

func (c *Config) removeHosts(hosts []Entry) error {
	if len(hosts) == 0 {
		return nil
	}

	c.Lock.Lock()
	defer c.Lock.Unlock()

	for i := 0; i < len(c.Hosts); i++ {
		if sets.New[Entry]().Insert(hosts...).Has(c.Hosts[i]) {
			c.Hosts = append(c.Hosts[:i], c.Hosts[i+1:]...)
			i--
		}
	}

	hostFile := GetHostFile()
	content, err2 := os.ReadFile(hostFile)
	if err2 != nil {
		return err2
	}
	if !strings.Contains(string(content), config.HostsKeyWord) {
		return nil
	}

	var retain []string
	reader := bufio.NewReader(bytes.NewReader(content))
	for {
		line, err := reader.ReadString('\n')
		var needsRemove bool
		if strings.Contains(line, config.HostsKeyWord) {
			for _, host := range hosts {
				if strings.Contains(line, host.IP) && strings.Contains(line, host.Domain) {
					needsRemove = true
					break
				}
			}
		}
		if !needsRemove {
			retain = append(retain, line)
		}
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
	}

	if len(retain) == 0 {
		plog.G(context.Background()).Errorf("Hosts files retain line is empty, should not happened")
		return nil
	}

	var sb = new(strings.Builder)
	for _, s := range retain {
		sb.WriteString(s)
	}
	str := strings.TrimSuffix(sb.String(), "\n")
	err := os.WriteFile(hostFile, []byte(str), 0644)
	return err
}

type Entry struct {
	IP     string
	Domain string
}

func entryList2String(entryList []Entry) string {
	var sb = new(bytes.Buffer)
	w := tabwriter.NewWriter(sb, 1, 1, 1, ' ', 0)
	for _, e := range entryList {
		_, _ = fmt.Fprintf(w, "\n%s\t%s\t%s\t%s", e.IP, e.Domain, "", config.HostsKeyWord)
	}
	_ = w.Flush()
	return sb.String()
}

func (c *Config) generateAppendHosts(serviceList []v12.Service, hosts []Entry) []Entry {
	const ServiceKubernetes = "kubernetes"
	var entryList = sets.New[Entry]().Insert(c.Hosts...).Insert(hosts...).UnsortedList()

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
		if !sets.New[Entry]().Insert(entryList...).Has(e) {
			entryList = append([]Entry{e}, entryList...)
		}
	}

	// 2) if hosts file already contains item, not needs to add it to hosts file
	hostFile := GetHostFile()
	content, err2 := os.ReadFile(hostFile)
	if err2 == nil {
		reader := bufio.NewReader(strings.NewReader(string(content)))
		for {
			line, err := reader.ReadString('\n')
			for i := 0; i < len(entryList); i++ {
				if strings.Contains(line, config.HostsKeyWord) && strings.Contains(line, entryList[i].Domain) {
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

func CleanupHosts() error {
	path := GetHostFile()
	content, err2 := os.ReadFile(path)
	if err2 != nil {
		return err2
	}
	if !strings.Contains(string(content), config.HostsKeyWord) {
		return nil
	}

	var retain []string
	reader := bufio.NewReader(bytes.NewReader(content))
	for {
		line, err := reader.ReadString('\n')
		if !strings.Contains(line, config.HostsKeyWord) {
			retain = append(retain, line)
		}
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
	}
	if len(retain) == 0 {
		return fmt.Errorf("empty hosts file")
	}

	var sb strings.Builder
	for _, s := range retain {
		sb.WriteString(s)
	}
	str := strings.TrimSuffix(sb.String(), "\n")
	err2 = os.WriteFile(path, []byte(str), 0644)
	return err2
}
