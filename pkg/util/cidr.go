package util

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/containernetworking/cni/libcni"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// GetCIDR discovers cluster Pod and Service CIDRs using multiple strategies:
// component flags in kube-system pods, CNI config files, and the service IP range error trick.
func GetCIDR(ctx context.Context, clientset kubernetes.Interface, restconfig *rest.Config, namespace string, image string) []*net.IPNet {
	defer func() {
		_ = clientset.CoreV1().Pods(namespace).Delete(context.Background(), config.CniNetName, v1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
	}()

	var result []*net.IPNet
	plog.StepStart(ctx, "Detecting cluster CIDRs")
	plog.G(ctx).Debugln("Detecting cluster CIDRs from component flags...")
	info, err := GetCIDRByDumpClusterInfo(ctx, clientset)
	if err == nil {
		plog.G(ctx).Debugf("Detected CIDRs from component flags")
		result = append(result, info...)
	}

	plog.G(ctx).Debugln("Detecting cluster CIDRs from CNI config...")
	cni, err := GetCIDRFromCNI(ctx, clientset, restconfig, namespace, image)
	if err == nil {
		plog.G(ctx).Debugf("Detected CIDRs from CNI config")
		result = append(result, cni...)
	}

	podCIDR, err := GetPodCIDRFromCNI(ctx, clientset, restconfig, namespace)
	if err == nil {
		result = append(result, podCIDR...)
	}
	plog.StepDone(ctx, "Detected cluster CIDRs: %s", CIDRsToString(result))

	plog.StepStart(ctx, "Detecting service CIDR")
	svcCIDR, _ := GetServiceCIDRByCreateService(ctx, clientset.CoreV1().Services(namespace))
	if svcCIDR != nil {
		plog.StepDone(ctx, "Detected service CIDR: %s", svcCIDR.String())
		result = append(result, svcCIDR)

		podCIDR, err = GetPodCIDRFromPod(ctx, clientset, namespace, svcCIDR)
		if err == nil {
			result = append(result, podCIDR...)
		}
	} else {
		plog.StepDone(ctx, "Detected service CIDR: (none)")
	}

	return result
}

// CIDRsToString joins CIDRs into a comma-separated string, or "(none)" if empty.
func CIDRsToString(cidrs []*net.IPNet) string {
	if len(cidrs) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(cidrs))
	for _, c := range cidrs {
		if c != nil {
			parts = append(parts, c.String())
		}
	}
	return strings.Join(parts, ", ")
}

// parseCIDRFromFlag extracts CIDRs from Kubernetes component command-line flags
// (--cluster-cidr and --service-cluster-ip-range).
func parseCIDRFromFlag(content string) (result []*net.IPNet) {
	if strings.Contains(content, "cluster-cidr") || strings.Contains(content, "service-cluster-ip-range") {
		_, cidrList, found := strings.Cut(content, "=")
		if found {
			for _, cidr := range strings.Split(cidrList, ",") {
				_, c, _ := net.ParseCIDR(cidr)
				if c != nil {
					result = append(result, c)
				}
			}
		}
	}
	return
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

// GetCIDRFromCNI extracts CIDRs by reading host /proc/*/cmdline inside a helper pod
// and grepping for --cluster-cidr and --service-cluster-ip-range flags.
func GetCIDRFromCNI(ctx context.Context, clientset kubernetes.Interface, restconfig *rest.Config, namespace string, image string) ([]*net.IPNet, error) {
	pod, err := CreateCIDRPod(ctx, clientset, namespace, image)
	if err != nil {
		return nil, err
	}

	cmd := `grep -a -R "service-cluster-ip-range\|cluster-cidr" /etc/cni/proc/*/cmdline | grep -a -v grep | tr "\0" "\n"`

	var content string
	content, err = Shell(ctx, clientset, restconfig, pod.Name, "", pod.Namespace, []string{"sh", "-c", cmd})
	if err != nil {
		return nil, err
	}

	var result []*net.IPNet
	for _, s := range strings.Split(content, "\n") {
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

// GetPodCIDRFromCNI reads CNI conflist files from the helper pod and extracts
// pod CIDRs from IPAM configuration (e.g. Calico ipv4_pools/ipv6_pools).
func GetPodCIDRFromCNI(ctx context.Context, clientset kubernetes.Interface, restconfig *rest.Config, namespace string) ([]*net.IPNet, error) {
	content, err := Shell(ctx, clientset, restconfig, config.CniNetName, "", namespace, []string{"cat", "/etc/cni/net.d/*.conflist"})
	if err != nil {
		return nil, err
	}

	configList, err := libcni.ConfListFromBytes([]byte(content))
	if err != nil {
		return nil, err
	}
	plog.G(ctx).Debugln("Found CNI config", configList.Name)
	var cidrList []*net.IPNet
	for _, plugin := range configList.Plugins {
		switch plugin.Network.Type {
		case "calico":
			m := map[string]any{}
			_ = json.Unmarshal(plugin.Bytes, &m)
			slice, _, _ := unstructured.NestedStringSlice(m, "ipam", "ipv4_pools")
			slice6, _, _ := unstructured.NestedStringSlice(m, "ipam", "ipv6_pools")
			for _, s := range sets.New[string]().Insert(slice...).Insert(slice6...).UnsortedList() {
				if _, ipNet, _ := net.ParseCIDR(s); ipNet != nil {
					cidrList = append(cidrList, ipNet)
				}
			}
		}
	}

	return cidrList, nil
}

// CreateCIDRPod creates a helper pod that mounts /etc/cni and /proc from the host for CIDR discovery.
func CreateCIDRPod(ctx context.Context, clientset kubernetes.Interface, namespace string, image string) (*corev1.Pod, error) {
	pod := buildCIDRPodSpec(namespace, image)
	get, err := clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, v1.GetOptions{})
	if err != nil || get.Status.Phase != corev1.PodRunning {
		if err == nil || !errors.IsNotFound(err) {
			_ = clientset.CoreV1().Pods(namespace).Delete(ctx, pod.Name, v1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
		}
		pod, err = clientset.CoreV1().Pods(namespace).Create(ctx, pod, v1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		field := fields.OneTermEqualSelector("metadata.name", pod.Name).String()
		const podWaitTimeout = 15 * time.Second
		ctx2, cancelFunc := context.WithTimeout(ctx, podWaitTimeout)
		defer cancelFunc()
		err = WaitPod(ctx2, clientset.CoreV1().Pods(namespace), v1.ListOptions{FieldSelector: field}, func(pod *corev1.Pod) bool {
			return pod.Status.Phase == corev1.PodRunning
		})
		if err != nil {
			return nil, err
		}
	}
	return pod, nil
}

func buildCIDRPodSpec(namespace, image string) *corev1.Pod {
	procName := "proc-dir-kubevpn"
	return &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      config.CniNetName,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: config.CniNetName,
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: config.DefaultNetDir,
							Type: ptr.To[corev1.HostPathType](corev1.HostPathDirectoryOrCreate),
						},
					},
				},
				{
					Name: procName,
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: config.Proc,
							Type: ptr.To[corev1.HostPathType](corev1.HostPathDirectoryOrCreate),
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:    config.CniNetName,
					Image:   image,
					Command: []string{"tail", "-f", "/dev/null"},
					Resources: corev1.ResourceRequirements{
						Requests: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceCPU:    resource.MustParse("16m"),
							corev1.ResourceMemory: resource.MustParse("16Mi"),
						},
						Limits: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceCPU:    resource.MustParse("16m"),
							corev1.ResourceMemory: resource.MustParse("16Mi"),
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      config.CniNetName,
							ReadOnly:  true,
							MountPath: config.DefaultNetDir,
						},
						{
							Name:      procName,
							ReadOnly:  true,
							MountPath: "/etc/cni" + config.Proc,
						},
					},
					ImagePullPolicy: corev1.PullIfNotPresent,
				},
			},
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
						{
							Weight: 50,
							Preference: corev1.NodeSelectorTerm{
								MatchExpressions: []corev1.NodeSelectorRequirement{
									{
										Key:      "node-role.kubernetes.io/master",
										Operator: corev1.NodeSelectorOpExists,
									},
									{
										Key:      "node-role.kubernetes.io/control-plane",
										Operator: corev1.NodeSelectorOpExists,
									},
								},
							},
						},
					},
				},
			},
			Tolerations: []corev1.Toleration{
				{
					Key:      "node-role.kubernetes.io/master",
					Operator: corev1.TolerationOpEqual,
					Effect:   corev1.TaintEffectNoSchedule,
				}, {
					Key:      "node-role.kubernetes.io/control-plane",
					Operator: corev1.TolerationOpEqual,
					Effect:   corev1.TaintEffectNoSchedule,
				},
			},
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
				{
					MaxSkew:           1,
					TopologyKey:       "kubernetes.io/hostname",
					WhenUnsatisfiable: corev1.ScheduleAnyway,
					LabelSelector:     v1.SetAsLabelSelector(map[string]string{"app": config.ConfigMapPodTrafficManager}),
				},
			},
		},
	}
}

// GetPodCIDRFromPod infers pod CIDRs by applying a default pod CIDR mask to actual pod IPs in the namespace.
// It uses /16 for IPv4 and /64 for IPv6 as safe defaults that cover most Kubernetes deployments,
// since the exact pod CIDR mask is not available without node.Spec.PodCIDR.
func GetPodCIDRFromPod(ctx context.Context, clientset kubernetes.Interface, namespace string, _ *net.IPNet) ([]*net.IPNet, error) {
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
				var mask net.IPMask
				if ip.To4() != nil {
					mask = net.CIDRMask(16, 32)
				} else {
					mask = net.CIDRMask(64, 128)
				}
				_, ipNet, _ := net.ParseCIDR((&net.IPNet{IP: ip, Mask: mask}).String())
				if ipNet != nil {
					result = append(result, ipNet)
				}
			}
		}
	}

	return result, nil
}

// RemoveCIDRsContainingIPs removes any CIDR from the slice that contains one of the given IPs.
func RemoveCIDRsContainingIPs(cidrs []*net.IPNet, ipList []net.IP) []*net.IPNet {
	for i := len(cidrs) - 1; i >= 0; i-- {
		for _, ip := range ipList {
			if cidrs[i].Contains(ip) {
				cidrs = append(cidrs[:i], cidrs[i+1:]...)
				break
			}
		}
	}
	return cidrs
}

// GetAPIServerIP resolves the API server host URL to a list of IP addresses via parsing and DNS lookup.
func GetAPIServerIP(apiServerHost string) ([]net.IP, error) {
	u, err := url.Parse(apiServerHost)
	if err != nil {
		return nil, err
	}

	var host string
	if strings.IndexByte(u.Host, ':') < 0 {
		host = u.Host
	} else {
		host, _, err = net.SplitHostPort(u.Host)
		if err != nil {
			return nil, err
		}
	}

	var ipList []net.IP
	seen := sets.New[string]()
	addIP := func(ip net.IP) {
		key := ip.String()
		if !seen.Has(key) {
			ipList = append(ipList, ip)
			seen.Insert(key)
		}
	}

	if ip := net.ParseIP(host); ip != nil {
		addIP(ip)
	}

	addrs, _ := net.LookupHost(host)
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil {
			addIP(ip)
		}
	}
	return ipList, nil
}

// RemoveLargerOverlappingCIDRs deduplicates overlapping CIDRs, keeping only the broadest (smallest prefix) in each overlap group.
func RemoveLargerOverlappingCIDRs(cidrNets []*net.IPNet) []*net.IPNet {
	sort.Slice(cidrNets, func(i, j int) bool {
		onesI, _ := cidrNets[i].Mask.Size()
		onesJ, _ := cidrNets[j].Mask.Size()
		// mask number is smaller, means the mask is larger
		return onesI < onesJ
	})

	var cidrsOverlap = func(cidr1, cidr2 *net.IPNet) bool {
		return cidr1.Contains(cidr2.IP) || cidr2.Contains(cidr1.IP)
	}

	var result []*net.IPNet
	skipped := make(map[int]bool)

	for i := range cidrNets {
		if skipped[i] {
			continue
		}
		for j := i + 1; j < len(cidrNets); j++ {
			if cidrsOverlap(cidrNets[i], cidrNets[j]) {
				skipped[j] = true
			}
		}
		result = append(result, cidrNets[i])
	}
	return result
}
