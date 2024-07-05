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
	log "github.com/sirupsen/logrus"
	v12 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	v13 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

type Config struct {
	Config  *miekgdns.ClientConfig
	Ns      []string
	TunName string

	Hosts []Entry
	Lock  *sync.Mutex
}

func (c *Config) AddServiceNameToHosts(ctx context.Context, serviceInterface v13.ServiceInterface, hosts ...Entry) error {
	list, err := serviceInterface.List(ctx, v1.ListOptions{})
	if err != nil {
		return err
	}

	c.Lock.Lock()
	defer c.Lock.Unlock()

	appendHosts := c.generateAppendHosts(list.Items, hosts)
	err = c.appendHosts(appendHosts)
	if err != nil {
		log.Errorf("failed to add hosts(%s): %v", entryList2String(appendHosts), err)
		return err
	}

	go c.watchServiceToAddHosts(ctx, serviceInterface, hosts)
	return nil
}

func (c *Config) watchServiceToAddHosts(ctx context.Context, serviceInterface v13.ServiceInterface, hosts []Entry) {
	ticker := time.NewTicker(time.Second * 15)
	defer ticker.Stop()

	for ctx.Err() == nil {
		err := func() error {
			w, err := serviceInterface.Watch(ctx, v1.ListOptions{Watch: true})
			if err != nil {
				return err
			}
			defer w.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case event, ok := <-w.ResultChan():
					if !ok {
						return errors.New("watch service chan done")
					}
					svc, ok := event.Object.(*v12.Service)
					if !ok {
						continue
					}
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if event.Type == watch.Deleted {
						if net.ParseIP(svc.Spec.ClusterIP) == nil {
							continue
						}
						var list = []Entry{{
							IP:     svc.Spec.ClusterIP,
							Domain: svc.Name,
						}}
						err = c.removeHosts(list)
						if err != nil {
							log.Errorf("failed to remove hosts(%s) to hosts: %v", entryList2String(list), err)
						}
					}
					if event.Type == watch.Added {
						c.Lock.Lock()
						appendHosts := c.generateAppendHosts([]v12.Service{*svc}, hosts)
						err = c.appendHosts(appendHosts)
						c.Lock.Unlock()
						if err != nil {
							log.Errorf("failed to add hosts(%s) to hosts: %v", entryList2String(appendHosts), err)
						}
					}
				case <-ticker.C:
					var list *v12.ServiceList
					list, err = serviceInterface.List(ctx, v1.ListOptions{})
					if err != nil {
						continue
					}
					c.Lock.Lock()
					appendHosts := c.generateAppendHosts(list.Items, hosts)
					err = c.appendHosts(appendHosts)
					c.Lock.Unlock()
					if err != nil {
						log.Errorf("failed to add hosts(%s) to hosts: %v", entryList2String(appendHosts), err)
					}
				}
			}
		}()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Error(err)
		}
		if utilnet.IsConnectionRefused(err) || apierrors.IsTooManyRequests(err) || apierrors.IsForbidden(err) {
			time.Sleep(time.Second * 5)
		} else {
			time.Sleep(time.Second * 2)
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
		log.Errorf("hosts files retain line is empty, should not happened")
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
		if net.ParseIP(service.Spec.ClusterIP) == nil {
			continue
		}
		var e = Entry{IP: service.Spec.ClusterIP, Domain: service.Name}
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
