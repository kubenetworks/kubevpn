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
	"time"

	miekgdns "github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
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
	Lock  *sync.Mutex
}

func (c *Config) AddServiceNameToHosts(ctx context.Context, serviceInterface v13.ServiceInterface, hosts ...Entry) {
	rateLimiter := flowcontrol.NewTokenBucketRateLimiter(0.2, 1)
	defer rateLimiter.Stop()

	serviceList, err := serviceInterface.List(ctx, v1.ListOptions{})
	c.Lock.Lock()
	if err == nil && len(serviceList.Items) != 0 {
		hostsEntry := c.generateHostsEntry(serviceList.Items, hosts)
		if err = c.addHosts(hostsEntry); err != nil {
			log.Errorf("failed to add hosts(%s): %v", entryList2String(hostsEntry), err)
		}
	}
	c.Lock.Unlock()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				time.Sleep(time.Second * 5)

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
						event, ok := <-w.ResultChan()
						if !ok {
							return
						}
						if watch.Error == event.Type || watch.Bookmark == event.Type {
							continue
						}
						if !rateLimiter.TryAccept() {
							return
						}
						if event.Type == watch.Deleted {
							svc, ok := event.Object.(*v12.Service)
							if !ok {
								continue
							}
							var list []Entry
							for _, p := range sets.New[string](svc.Spec.ClusterIPs...).Insert(svc.Spec.ClusterIP).UnsortedList() {
								if net.ParseIP(p) == nil {
									continue
								}
								list = append(list, Entry{
									IP:     p,
									Domain: svc.Name,
								})
							}
							err = c.removeHosts(list)
							if err != nil {
								log.Errorf("failed to remove hosts(%s) to hosts: %v", entryList2String(list), err)
							}
							continue
						}
						list, err := serviceInterface.List(ctx, v1.ListOptions{})
						if err != nil {
							return
						}
						func() {
							c.Lock.Lock()
							defer c.Lock.Unlock()
							hostsEntry := c.generateHostsEntry(list.Items, hosts)
							err := c.addHosts(hostsEntry)
							if err != nil {
								log.Errorf("failed to add hosts(%s) to hosts: %v", entryList2String(hostsEntry), err)
							}
						}()
					}
				}()
			}
		}
	}()
}

// param: entry list is needs to added
// 1) check whether already exist, if exist not needs to add
// 2) check whether already can find this host, not needs to add
// 3) otherwise add it to hosts file
func (c *Config) addHosts(entryList []Entry) error {
	if len(entryList) == 0 {
		return nil
	}

	var sb = new(bytes.Buffer)
	w := tabwriter.NewWriter(sb, 1, 1, 1, ' ', 0)
	for _, e := range entryList {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.IP, e.Domain, "", config.HostsKeyWord)
	}
	_ = w.Flush()

	hostFile := GetHostFile()
	fi, err := os.OpenFile(hostFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer fi.Close()
	_, err = fi.WriteString(sb.String())
	return err
}

func (c *Config) removeHosts(entryList []Entry) error {
	c.Lock.Lock()
	defer c.Lock.Unlock()

	if len(entryList) == 0 {
		return nil
	}

	for _, entry := range entryList {
		for i := 0; i < len(c.Hosts); i++ {
			if entry == c.Hosts[i] {
				c.Hosts = append(c.Hosts[:i], c.Hosts[i+1:]...)
				i--
			}
		}
	}

	hostFile := GetHostFile()
	f, err := os.OpenFile(hostFile, os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	var retain []string
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		var needsRemove bool
		if strings.Contains(line, config.HostsKeyWord) {
			for _, host := range entryList {
				if strings.Contains(line, host.IP) && strings.Contains(line, host.Domain) {
					needsRemove = true
				}
			}
		}
		if !needsRemove {
			retain = append(retain, line)
		}
	}

	if len(retain) == 0 {
		log.Errorf("hosts files retain line is empty, should not happened")
		return nil
	}

	var sb = new(strings.Builder)
	for _, s := range retain {
		sb.WriteString(s)
	}
	err = f.Truncate(0)
	_, err = f.Seek(0, 0)
	_, err = f.WriteString(sb.String())
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
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.IP, e.Domain, "", config.HostsKeyWord)
	}
	_ = w.Flush()
	return sb.String()
}

func (c *Config) generateHostsEntry(list []v12.Service, hosts []Entry) []Entry {
	const ServiceKubernetes = "kubernetes"
	var entryList = sets.New[Entry]().Insert(c.Hosts...).Insert(hosts...).UnsortedList()

	// get all service ip
	for _, item := range list {
		if strings.EqualFold(item.Name, ServiceKubernetes) {
			continue
		}
		ipList := sets.New[string](item.Spec.ClusterIPs...).Insert(item.Spec.ClusterIP).Insert(item.Spec.ExternalIPs...).UnsortedList()
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
	sort.SliceStable(entryList, func(i, j int) bool {
		if entryList[i].Domain == entryList[j].Domain {
			return entryList[i].IP > entryList[j].IP
		}
		return entryList[i].Domain > entryList[j].Domain
	})

	// 1) if dns already works well, not needs to add it to hosts file
	var alreadyCanResolveDomain []Entry
	for i := 0; i < len(entryList); i++ {
		go func(e Entry) {
			timeout, cancelFunc := context.WithTimeout(context.Background(), time.Millisecond*1000)
			defer cancelFunc()
			// net.DefaultResolver.PreferGo = true
			host, err := net.DefaultResolver.LookupHost(timeout, e.Domain)
			if err == nil && sets.NewString(host...).Has(e.IP) {
				alreadyCanResolveDomain = append(alreadyCanResolveDomain, e)
			}
		}(entryList[i])
	}
	// remove those already can resolve domain from entryList
	for i := 0; i < len(entryList); i++ {
		for _, entry := range alreadyCanResolveDomain {
			if entryList[i] == entry {
				entryList = append(entryList[:i], entryList[i+1:]...)
				i--
				break
			}
		}
	}

	// 2) if hosts file already contains item, not needs to add it to hosts file
	hostFile := GetHostFile()
	content, err := os.ReadFile(hostFile)
	if err == nil {
		reader := bufio.NewReader(strings.NewReader(string(content)))
		for {
			readString, err := reader.ReadString('\n')
			if errors.Is(err, io.EOF) {
				break
			}
			for j := 0; j < len(entryList); j++ {
				entry := entryList[j]
				if strings.Contains(readString, entry.Domain) && strings.Contains(readString, entry.IP) {
					entryList = append(entryList[:j], entryList[j+1:]...)
					j--
				}
			}
		}
	}

	c.Hosts = entryList
	return entryList
}

func CleanupHosts() error {
	path := GetHostFile()
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	var retain []string
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if !strings.Contains(line, config.HostsKeyWord) {
			retain = append(retain, line)
		}
	}
	if len(retain) == 0 {
		return fmt.Errorf("empty hosts file")
	}

	var sb strings.Builder
	for _, s := range retain {
		sb.WriteString(s)
	}
	err = f.Truncate(0)
	_, err = f.Seek(0, 0)
	_, err = f.WriteString(sb.String())
	return err
}
