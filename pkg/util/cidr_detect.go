package util

import (
	"context"
	"fmt"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// GetCIDR discovers cluster Pod and Service CIDRs using pod-free strategies only:
// component flags in kube-system pods, the Service IP-range error trick, and inference from
// live Pod/Service IPs. It deliberately does NOT create a probe pod / exec (which added ~15s
// per connect) — trading some completeness on locked-down clusters for a fast connect. The
// server-side warm-up (GetClusterCIDRNoProbePod, see docs/46) uses the same strategy set.
//
// Detection is reported as three progress steps — cluster CIDRs (component flags), Pod CIDR
// (inferred from live Pod IPs), and Service CIDR (error trick + inferred from Service IPs) —
// so each is visible even when the others come up empty.
func GetCIDR(ctx context.Context, clientset kubernetes.Interface, namespace string) []*net.IPNet {
	var result []*net.IPNet

	plog.StepStart(ctx, "Detecting cluster CIDRs")
	var flags []*net.IPNet
	if info, err := GetCIDRByDumpClusterInfo(ctx, clientset); err == nil {
		flags = info
	}
	result = append(result, flags...)
	plog.StepDone(ctx, "Detected cluster CIDRs: %s", CIDRsToString(flags))

	plog.StepStart(ctx, "Detecting pod CIDR")
	var pod []*net.IPNet
	if podCIDR, err := GetPodCIDRFromPod(ctx, clientset, namespace); err == nil {
		pod = podCIDR
	}
	result = append(result, pod...)
	plog.StepDone(ctx, "Detected pod CIDR: %s", CIDRsToString(pod))

	plog.StepStart(ctx, "Detecting service CIDR")
	svc := detectServiceCIDRs(ctx, clientset, namespace)
	result = append(result, svc...)
	plog.StepDone(ctx, "Detected service CIDR: %s", CIDRsToString(svc))

	return result
}

// detectServiceCIDRs runs the two pod-free Service-CIDR strategies — the rejected-Service
// error trick (GetServiceCIDRByCreateService) and inferring from existing Services'
// ClusterIPs (GetServiceCIDRFromService) — and returns their union. Shared by GetCIDR and
// GetClusterCIDRNoProbePod so both report and route the same Service CIDRs.
func detectServiceCIDRs(ctx context.Context, clientset kubernetes.Interface, namespace string) []*net.IPNet {
	var svc []*net.IPNet
	if s, _ := GetServiceCIDRByCreateService(ctx, clientset.CoreV1().Services(namespace)); s != nil {
		svc = append(svc, s)
	}
	if s, err := GetServiceCIDRFromService(ctx, clientset, namespace); err == nil {
		svc = append(svc, s...)
	}
	return svc
}

// GetClusterCIDRNoProbePod detects cluster CIDRs WITHOUT creating a probe pod or
// exec-ing into one — suitable for running inside the cluster (e.g. the traffic
// manager's server-side warm-up, see docs/46). It combines only the pod-free
// strategies: component flags from kube-system pod specs, the Service CIDR from a
// rejected Service create + inference from Service ClusterIPs, and the Pod CIDR
// inferred from existing pod IPs. Any strategy lacking RBAC simply contributes nothing.
func GetClusterCIDRNoProbePod(ctx context.Context, clientset kubernetes.Interface, namespace string) []*net.IPNet {
	var result []*net.IPNet
	if info, err := GetCIDRByDumpClusterInfo(ctx, clientset); err == nil {
		result = append(result, info...)
	}
	result = append(result, detectServiceCIDRs(ctx, clientset, namespace)...)
	if podCIDR, err := GetPodCIDRFromPod(ctx, clientset, namespace); err == nil {
		result = append(result, podCIDR...)
	}
	return result
}

// GetCIDRByDumpClusterInfo extracts CIDRs from kube-system pod command-line flags
// (kube-apiserver --service-cluster-ip-range, kube-controller-manager --cluster-cidr, etc.).
func GetCIDRByDumpClusterInfo(ctx context.Context, clientset kubernetes.Interface) ([]*net.IPNet, error) {
	podList, err := clientset.CoreV1().Pods(v1.NamespaceSystem).List(ctx, v1.ListOptions{Limit: 100})
	if err != nil {
		return nil, err
	}
	var list []string
	for _, item := range podList.Items {
		for _, container := range item.Spec.Containers {
			list = append(list, container.Args...)
			list = append(list, container.Command...)
		}
	}

	var result []*net.IPNet
	for _, s := range list {
		result = append(result, parseCIDRFromFlag(s)...)
	}
	return result, nil
}

// GetServiceCIDRByCreateService discovers the service CIDR by attempting to create a service with an invalid ClusterIP and parsing the error.
func GetServiceCIDRByCreateService(ctx context.Context, serviceInterface typedcorev1.ServiceInterface) (*net.IPNet, error) {
	cidrKeywords := []string{
		"valid IPs is",
		"The range of valid IPs",
		"valid IP range is",
	}
	svc := &corev1.Service{
		ObjectMeta: v1.ObjectMeta{GenerateName: "foo-svc-"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}, ClusterIP: "0.0.0.0"},
	}
	_, err := serviceInterface.Create(ctx, svc, v1.CreateOptions{})
	if err != nil {
		errMsg := err.Error()
		for _, keyword := range cidrKeywords {
			idx := strings.LastIndex(errMsg, keyword)
			if idx != -1 {
				_, cidr, parseErr := net.ParseCIDR(strings.TrimSpace(errMsg[idx+len(keyword):]))
				if parseErr == nil && cidr != nil {
					return cidr, nil
				}
			}
		}
		return nil, fmt.Errorf("cannot detect service CIDR from error message: %w", err)
	}
	return nil, fmt.Errorf("cannot detect service CIDR: service creation did not return expected error")
}

// inferredCIDRFromIP expands a single cluster IP to its inferred containing CIDR:
// /24 for IPv4, /64 for IPv6. Used as a fallback when the exact CIDR mask is not
// available (e.g. without node.Spec.PodCIDR or the Service IP range). /24 is deliberately
// narrow so a routed range does not hijack locally-used networks; each observed IP yields
// its own /24 and callers dedup/merge overlaps via RemoveLargerOverlappingCIDRs.
func inferredCIDRFromIP(ip net.IP) *net.IPNet {
	var mask net.IPMask
	if ip.To4() != nil {
		mask = net.CIDRMask(24, 32)
	} else {
		mask = net.CIDRMask(64, 128)
	}
	_, ipNet, _ := net.ParseCIDR((&net.IPNet{IP: ip, Mask: mask}).String())
	return ipNet
}

// GetPodCIDRFromPod infers pod CIDRs by applying a default pod CIDR mask to actual pod IPs in the namespace.
// It uses /24 for IPv4 and /64 for IPv6 (see inferredCIDRFromIP), since the exact pod CIDR
// mask is not available without node.Spec.PodCIDR.
func GetPodCIDRFromPod(ctx context.Context, clientset kubernetes.Interface, namespace string) ([]*net.IPNet, error) {
	podList, err := clientset.CoreV1().Pods(namespace).List(ctx, v1.ListOptions{Limit: 100})
	if err != nil {
		return nil, err
	}

	var result []*net.IPNet
	for _, item := range podList.Items {
		if item.Spec.HostNetwork {
			continue
		}

		s := sets.New[string]().Insert(item.Status.PodIP)
		for _, p := range item.Status.PodIPs {
			s.Insert(p.IP)
		}
		for _, t := range s.UnsortedList() {
			if ip := net.ParseIP(t); ip != nil {
				if ipNet := inferredCIDRFromIP(ip); ipNet != nil {
					result = append(result, ipNet)
				}
			}
		}
	}

	// A cluster generally has one Pod CIDR range; coalesce the per-IP /24s (bounded) so
	// pods in un-observed sub-ranges still route. See mergeToSupernet.
	return mergeToSupernet(result), nil
}

// GetServiceCIDRFromService infers service CIDRs by applying a default mask (/24 for IPv4,
// /64 for IPv6, see inferredCIDRFromIP) to the ClusterIPs of existing Services in the
// namespace. It is a pod-free fallback for GetServiceCIDRByCreateService: it needs only
// `services list` RBAC and does not depend on the API error-message wording. Headless
// ("None") and empty ClusterIPs are skipped.
func GetServiceCIDRFromService(ctx context.Context, clientset kubernetes.Interface, namespace string) ([]*net.IPNet, error) {
	svcList, err := clientset.CoreV1().Services(namespace).List(ctx, v1.ListOptions{Limit: 100})
	if err != nil {
		return nil, err
	}

	seen := sets.New[string]()
	var result []*net.IPNet
	for _, item := range svcList.Items {
		ips := sets.New[string]().Insert(item.Spec.ClusterIP).Insert(item.Spec.ClusterIPs...)
		for _, s := range ips.UnsortedList() {
			if s == "" || s == corev1.ClusterIPNone {
				continue
			}
			ip := net.ParseIP(s)
			if ip == nil {
				continue
			}
			ipNet := inferredCIDRFromIP(ip)
			if ipNet == nil || seen.Has(ipNet.String()) {
				continue
			}
			seen.Insert(ipNet.String())
			result = append(result, ipNet)
		}
	}

	// A cluster generally has one Service CIDR range; coalesce the per-ClusterIP /24s
	// (bounded) so services in un-observed sub-ranges still route. See mergeToSupernet.
	return mergeToSupernet(result), nil
}
