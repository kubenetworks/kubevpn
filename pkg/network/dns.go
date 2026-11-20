package network

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type dnsState struct {
	extraHost []dns.Entry
	dnsConfig *dns.Config
}

// GetExtraHost returns the extra DNS host entries accumulated by AddExtraRoute.
func (nm *Runtime) GetExtraHost() []dns.Entry {
	return nm.dns.extraHost
}

// setupDNS configures DNS resolution for the cluster.
func (nm *Runtime) setupDNS(ctx context.Context) error {
	podList, err := nm.cfg.GetRunningPodList(ctx)
	if err != nil {
		plog.G(ctx).Errorf("Get running pod list failed, err: %v", err)
		return err
	}
	pod := podList[0]
	plog.StepStart(ctx, "Configuring DNS")
	plog.G(ctx).Debugf("Getting DNS service IP from pod...")
	relovConf, err := util.GetDNSServiceIPFromPod(ctx, nm.cfg.Clientset, nm.cfg.Config, pod.GetName(), nm.cfg.ManagerNamespace)
	if err != nil {
		plog.G(ctx).Errorln(err)
		return err
	}

	marshal, _ := json.Marshal(relovConf)
	plog.G(ctx).Debugf("Get DNS service config: %v", string(marshal))
	var svc *v1.Service
	svc, err = nm.cfg.Clientset.CoreV1().Services(nm.cfg.ManagerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}
	err = detectNameserver(ctx, relovConf, svc.Spec.ClusterIP, pod.Status.PodIP)
	if err != nil {
		return err
	}

	plog.G(ctx).Debugf("Adding extra domains to hosts...")
	if err = nm.AddExtraRoute(ctx, relovConf.Servers); err != nil {
		plog.G(ctx).Errorf("Add extra route failed: %v", err)
		return err
	}

	ns := nm.listResolvableNamespaces(ctx)

	// Extend search domains to include all namespaces for cross-namespace short-name resolution
	// This fixes the case where user connects to "kubevpn" namespace but proxies workloads
	// in "default" namespace: short name "reviews" resolves as "reviews.default.svc.cluster.local"
	relovConf.Search = extendSearchDomains(relovConf.Search, ns)

	plog.G(ctx).Debugf("Listing namespace %s services...", nm.cfg.WorkloadNamespace)
	nm.dns.dnsConfig = &dns.Config{
		Config:   relovConf,
		Ns:       ns,
		Services: []v1.Service{},
		TunName:  nm.device.tunName,
		Hosts:    nm.dns.extraHost,
		Lock:     nm.cfg.Lock,
		HowToGetExternalName: func(domain string) (string, error) {
			ip, err := resolveDomainViaClusterDNS(ctx, relovConf.Servers, domain)
			if err != nil {
				return "", err
			}
			return ip.String(), nil
		},
	}
	plog.G(ctx).Debugf("Setting up DNS resolver on device %s...", nm.device.tunName)
	if err = nm.dns.dnsConfig.SetupDNS(ctx); err != nil {
		return err
	}
	if len(relovConf.Servers) > 0 {
		plog.StepDone(ctx, "Configured DNS (cluster DNS %s)", relovConf.Servers[0])
	} else {
		plog.StepDone(ctx, "Configured DNS")
	}

	plog.StepStart(ctx, "Writing service records to the hosts file")
	// dump service in current namespace for support DNS resolve service:port
	n, err := nm.dns.dnsConfig.AddServiceNameToHosts(ctx, nm.dns.extraHost...)
	if err != nil {
		return err
	}
	plog.StepDone(ctx, "Wrote %d service records to the hosts file (namespace %q)", n, nm.cfg.WorkloadNamespace)
	return nil
}

// listResolvableNamespaces returns the full list of namespaces to use for DNS resolution.
// WorkloadNamespace is always first. All other namespaces are appended in list order,
// skipping any duplicate. When the List call fails, only WorkloadNamespace is returned.
func (nm *Runtime) listResolvableNamespaces(ctx context.Context) []string {
	ns := []string{nm.cfg.WorkloadNamespace}
	list, err := nm.cfg.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 500})
	if err == nil {
		for _, item := range list.Items {
			if !sets.New[string](ns...).Has(item.Name) {
				ns = append(ns, item.Name)
			}
		}
	}
	return ns
}

// extendSearchDomains expands the search domain list to include suffixes for all known namespaces.
// This enables short-name resolution for services in namespaces that are being proxied but
// are different from the connect namespace.
func extendSearchDomains(search []string, namespaces []string) []string {
	if len(namespaces) <= 1 {
		return search
	}

	// Build a set for deduplication
	seen := make(map[string]bool)
	for _, s := range search {
		seen[s] = true
	}

	result := slices.Clone(search)

	// Extract the base domain suffixes from the original search list
	// e.g., "kubevpn.svc.cluster.local" → ["svc.cluster.local", "cluster.local"]
	var baseSuffixes []string
	for _, s := range search {
		parts := strings.SplitN(s, ".", 2)
		if len(parts) == 2 && parts[1] != "" {
			if !slices.Contains(baseSuffixes, parts[1]) {
				baseSuffixes = append(baseSuffixes, parts[1])
			}
		}
	}

	// If we couldn't extract base suffixes, use default Kubernetes suffixes
	if len(baseSuffixes) == 0 {
		baseSuffixes = []string{"svc.cluster.local", "cluster.local"}
	}

	// Add search domains for all namespaces
	for _, ns := range namespaces {
		// Skip the workload namespace since it's already in the original search
		if ns == namespaces[0] {
			continue
		}
		for _, suffix := range baseSuffixes {
			nsSearch := ns + "." + suffix
			if !seen[nsSearch] {
				seen[nsSearch] = true
				result = append(result, nsSearch)
			}
		}
	}

	return result
}
