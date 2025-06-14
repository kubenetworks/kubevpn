//go:build darwin

package dns

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	miekgdns "github.com/miekg/dns"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// https://github.com/golang/go/issues/12524
// man 5 resolver

var cancel context.CancelFunc
var resolv = "/etc/resolv.conf"
var ignoreSearchSuffix = []string{"com", "io", "net", "org", "cn", "ru"}

// SetupDNS support like
// service:port
// service.namespace:port
// service.namespace.svc:port
// service.namespace.svc.cluster:port
// service.namespace.svc.cluster.local:port
func (c *Config) SetupDNS(ctx context.Context) error {
	defer util.HandleCrash()
	ticker := time.NewTicker(time.Second * 15)
	_, err := c.SvcInformer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			if svc, ok := obj.(*v12.Service); ok && svc.Namespace == c.Ns[0] {
				return true
			} else {
				return false
			}
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				ticker.Reset(time.Second * 3)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				ticker.Reset(time.Second * 3)
			},
			DeleteFunc: func(obj interface{}) {
				ticker.Reset(time.Second * 3)
			},
		},
	})
	if err != nil {
		plog.G(ctx).Errorf("Failed to add service event handler: %v", err)
		return err
	}
	go func() {
		defer ticker.Stop()
		for ; ctx.Err() == nil; <-ticker.C {
			ticker.Reset(time.Second * 15)
			serviceList, err := c.SvcInformer.GetIndexer().ByIndex(cache.NamespaceIndex, c.Ns[0])
			if err != nil {
				plog.G(ctx).Errorf("Failed to list service by namespace %s: %v", c.Ns[0], err)
				continue
			}
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
			c.Services = services
			c.usingResolver(ctx)
		}
	}()
	c.usingResolver(ctx)
	return nil
}

func (c *Config) usingResolver(ctx context.Context) {
	var clientConfig = c.Config

	path := "/etc/resolver"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err = os.MkdirAll(path, 0755); err != nil {
			plog.G(ctx).Errorf("Create resolver error: %v", err)
		}
		if err = os.Chmod(path, 0755); err != nil {
			plog.G(ctx).Errorf("Chmod resolver error: %v", err)
		}
	}
	newConfig := miekgdns.ClientConfig{
		Servers: clientConfig.Servers,
		Search:  []string{},
		Port:    clientConfig.Port,
		Ndots:   clientConfig.Ndots,
		Timeout: clientConfig.Timeout,
	}
	for _, filename := range GetResolvers(c.Config.Search, c.Ns, c.Services) {
		// ignore search suffix like com, io, net, org, cn, ru, those are top dns server
		if slices.Contains(ignoreSearchSuffix, filepath.Base(filename)) {
			continue
		}
		content, err := os.ReadFile(filename)
		if os.IsNotExist(err) {
			_ = os.WriteFile(filename, []byte(toString(newConfig)), 0644)
			continue
		}
		if err != nil {
			plog.G(ctx).Errorf("Failed to read resovler %s error: %v", filename, err)
			continue
		}

		var conf *miekgdns.ClientConfig
		conf, err = miekgdns.ClientConfigFromReader(bytes.NewBufferString(string(content)))
		if err != nil {
			plog.G(ctx).Errorf("Parse resolver %s error: %v", filename, err)
			continue
		}
		if slices.Contains(conf.Servers, clientConfig.Servers[0]) {
			continue
		}
		// insert current name server to first location
		conf.Servers = append([]string{clientConfig.Servers[0]}, conf.Servers...)
		err = os.WriteFile(filename, []byte(toString(*conf)), 0644)
		if err != nil {
			plog.G(ctx).Errorf("Failed to write resovler %s error: %v", filename, err)
		}
	}
}

func (c *Config) usingNetworkSetup(ip string, ns string) {
	networkSetup(ip, ns)
	var ctx context.Context
	ctx, cancel = context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(time.Second * 10)
		newWatcher, _ := fsnotify.NewWatcher()
		defer newWatcher.Close()
		defer ticker.Stop()
		_ = newWatcher.Add(resolv)
		c := make(chan struct{}, 1)
		c <- struct{}{}
		for {
			select {
			case <-ticker.C:
				c <- struct{}{}
			case /*e :=*/ <-newWatcher.Events:
				//if e.Op == fsnotify.Write {
				c <- struct{}{}
				//}
			case <-c:
				if rc, err := miekgdns.ClientConfigFromFile(resolv); err == nil && rc.Timeout != 1 {
					if !sets.New[string](rc.Servers...).Has(ip) {
						rc.Servers = append(rc.Servers, ip)
						for _, s := range []string{ns + ".svc.cluster.local", "svc.cluster.local", "cluster.local"} {
							rc.Search = append(rc.Search, s)
						}
						//rc.Ndots = 5
					}
					//rc.Attempts = 1
					rc.Timeout = 1
					_ = os.WriteFile(resolv, []byte(toString(*rc)), 0644)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

func toString(config miekgdns.ClientConfig) string {
	var builder strings.Builder
	//	builder.WriteString(`#
	//# macOS Notice
	//#
	//# This file is not consulted for DNS hostname resolution, address
	//# resolution, or the DNS query routing mechanism used by most
	//# processes on this system.
	//#
	//# To view the DNS configuration used by this system, use:
	//#   scutil --dns
	//#
	//# SEE ALSO
	//#   dns-sd(1), scutil(8)
	//#
	//# This file is automatically generated.
	//#`)
	//	builder.WriteString("\n")
	if len(config.Search) > 0 {
		builder.WriteString(fmt.Sprintf("search %s\n", strings.Join(config.Search, " ")))
	}
	for i := range config.Servers {
		builder.WriteString(fmt.Sprintf("nameserver %s\n", config.Servers[i]))
	}
	if len(config.Port) != 0 {
		builder.WriteString(fmt.Sprintf("port %s\n", config.Port))
	}
	builder.WriteString(fmt.Sprintf("options ndots:%d\n", config.Ndots))
	builder.WriteString(fmt.Sprintf("options timeout:%d\n", config.Timeout))
	//builder.WriteString(fmt.Sprintf("options attempts:%d\n", config.Attempts))
	return builder.String()
}

func (c *Config) CancelDNS() {
	if cancel != nil {
		cancel()
	}
	for _, filename := range GetResolvers(c.Config.Search, c.Ns, c.Services) {
		content, err := os.ReadFile(filename)
		if err != nil {
			continue
		}
		var conf *miekgdns.ClientConfig
		conf, err = miekgdns.ClientConfigFromReader(bytes.NewBufferString(strings.TrimSpace(string(content))))
		if err != nil {
			continue
		}
		// if not has this DNS server, do nothing
		if !sets.New[string](conf.Servers...).Has(c.Config.Servers[0]) {
			continue
		}
		// reverse delete
		for i := len(conf.Servers) - 1; i >= 0; i-- {
			if conf.Servers[i] == c.Config.Servers[0] {
				conf.Servers = append(conf.Servers[:i], conf.Servers[i+1:]...)
				i--
				// remove once is enough, because if same cluster connect to different namespace
				// dns service ip is same
				break
			}
		}
		if len(conf.Servers) == 0 {
			_ = os.Remove(filename)
			continue
		}
		err = os.WriteFile(filename, []byte(toString(*conf)), 0644)
		if err != nil {
			plog.G(context.Background()).Errorf("Failed to write resovler %s error: %v", filename, err)
		}
	}
	//networkCancel()
	_ = c.removeHosts(sets.New[Entry]().Insert(c.Hosts...).UnsortedList())
}

// GetResolvers
// service name: authors
// namespace: test
// create resolvers suffix:
// [authors.test.svc.cluster.local]
// local
// cluster
// cluster.local
// svc.cluster
// test.svc
func GetResolvers(searchList []string, nsList []string, serviceName []v12.Service) []string {
	result := sets.New[string]().Insert(searchList...).Insert(nsList...)

	const splitter = "."
	// support default.svc, cluster.local
	for _, s := range searchList {
		split := strings.Split(s, splitter)
		for i := range len(split) - 1 {
			result.Insert(fmt.Sprintf("%s.%s", split[i], split[i+1]))
		}
		result.Insert(split...)
	}
	// authors.default
	for _, service := range serviceName {
		result.Insert(fmt.Sprintf("%s.%s", service.Name, service.Namespace))
	}

	var resolvers []string
	for _, s := range sets.List(result) {
		resolvers = append(resolvers, filepath.Join("/etc/resolver/", s))
	}
	return resolvers
}

/*
➜  resolver sudo networksetup -setdnsservers Wi-Fi 172.20.135.131 1.1.1.1
➜  resolver sudo networksetup -setsearchdomains Wi-Fi test.svc.cluster.local svc.cluster.local cluster.local
➜  resolver sudo networksetup -getsearchdomains Wi-Fi
test.svc.cluster.local
svc.cluster.local
cluster.local
➜  resolver sudo networksetup -getdnsservers Wi-Fi
172.20.135.131
1.1.1.1
*/
func networkSetup(ip string, namespace string) {
	networkCancel()
	b, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return
	}
	services := strings.Split(string(b), "\n")
	for _, s := range services[:len(services)-1] {
		cmd := exec.Command("networksetup", "-getdnsservers", s)
		output, err := cmd.Output()
		if err == nil {
			var nameservers []string
			if strings.Contains(string(output), "There aren't any DNS Servers") {
				nameservers = make([]string, 0, 0)
				// fix networksetup -getdnsservers is empty, but resolv.conf nameserver is not empty
				if rc, err := miekgdns.ClientConfigFromFile(resolv); err == nil {
					nameservers = rc.Servers
				}
			} else {
				nameservers = strings.Split(string(output), "\n")
				nameservers = nameservers[:len(nameservers)-1]
			}
			// add to tail
			nameservers = append(nameservers, ip)
			args := []string{"-setdnsservers", s}
			output, err = exec.Command("networksetup", append(args, nameservers...)...).Output()
			if err != nil {
				plog.G(context.Background()).Warnf("Failed to set DNS server for %s, err: %v, output: %s\n", s, err, string(output))
			}
		}
		output, err = exec.Command("networksetup", "-getsearchdomains", s).Output()
		if err == nil {
			var searchDomains []string
			if strings.Contains(string(output), "There aren't any Search Domains") {
				searchDomains = make([]string, 0, 0)
			} else {
				searchDomains = strings.Split(string(output), "\n")
				searchDomains = searchDomains[:len(searchDomains)-1]
			}
			newSearchDomains := make([]string, len(searchDomains)+3, len(searchDomains)+3)
			copy(newSearchDomains[3:], searchDomains)
			newSearchDomains[0] = fmt.Sprintf("%s.svc.cluster.local", namespace)
			newSearchDomains[1] = "svc.cluster.local"
			newSearchDomains[2] = "cluster.local"
			args := []string{"-setsearchdomains", s}
			bytes, err := exec.Command("networksetup", append(args, newSearchDomains...)...).Output()
			if err != nil {
				plog.G(context.Background()).Warnf("Failed to set search domain for %s, err: %v, output: %s\n", s, err, string(bytes))
			}
		}
	}
}

func networkCancel() {
	b, err := exec.Command("networksetup", "-listallnetworkservices").CombinedOutput()
	if err != nil {
		return
	}
	services := strings.Split(string(b), "\n")
	for _, s := range services[:len(services)-1] {
		output, err := exec.Command("networksetup", "-getsearchdomains", s).Output()
		if err == nil {
			i := strings.Split(string(output), "\n")
			if i[1] == "svc.cluster.local" && i[2] == "cluster.local" {
				bytes, err := exec.Command("networksetup", "-setsearchdomains", s, strings.Join(i[3:], " ")).Output()
				if err != nil {
					plog.G(context.Background()).Warnf("Failed to remove search domain for %s, err: %v, output: %s\n", s, err, string(bytes))
				}

				output, err := exec.Command("networksetup", "-getdnsservers", s).Output()
				if err == nil {
					dnsServers := strings.Split(string(output), "\n")
					// dnsServers[len(dnsServers)-1]=""
					// dnsServers[len(dnsServers)-2]="ip which added by KubeVPN"
					dnsServers = dnsServers[:len(dnsServers)-2]
					if len(dnsServers) == 0 {
						// set default dns server to 1.1.1.1 or just keep on empty
						dnsServers = append(dnsServers, "empty")
					}
					args := []string{"-setdnsservers", s}
					combinedOutput, err := exec.Command("networksetup", append(args, dnsServers...)...).Output()
					if err != nil {
						plog.G(context.Background()).Warnf("Failed to remove DNS server for %s, err: %v, output: %s", s, err, string(combinedOutput))
					}
				}
			}
		}
	}
}

func GetHostFile() string {
	return "/etc/hosts"
}
