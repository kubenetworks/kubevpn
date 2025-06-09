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
	v13 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// GetCIDR
// 1) dump cluster info
// 2) grep cmdline
// 3) create svc + cat *.conflist
// 4) create svc + get pod ip with svc mask
func GetCIDR(ctx context.Context, clientset *kubernetes.Clientset, restconfig *rest.Config, namespace string, image string) []*net.IPNet {
	defer func() {
		_ = clientset.CoreV1().Pods(namespace).Delete(context.Background(), config.CniNetName, v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)})
	}()

	var result []*net.IPNet
	plog.G(ctx).Infoln("Getting network CIDR from cluster info...")
	info, err := GetCIDRByDumpClusterInfo(ctx, clientset)
	if err == nil {
		plog.G(ctx).Debugf("Getting network CIDR from cluster info successfully")
		result = append(result, info...)
	}

	plog.G(ctx).Infoln("Getting network CIDR from CNI...")
	cni, err := GetCIDRFromCNI(ctx, clientset, restconfig, namespace, image)
	if err == nil {
		plog.G(ctx).Debugf("Getting network CIDR from CNI successfully")
		result = append(result, cni...)
	}

	podCIDR, err := GetPodCIDRFromCNI(ctx, clientset, restconfig, namespace)
	if err == nil {
		result = append(result, podCIDR...)
	}

	plog.G(ctx).Infoln("Getting network CIDR from services...")
	svcCIDR, _ := GetServiceCIDRByCreateService(ctx, clientset.CoreV1().Services(namespace))
	if svcCIDR != nil {
		plog.G(ctx).Debugf("Getting network CIDR from services successfully")
		result = append(result, svcCIDR)

		podCIDR, err = GetPodCIDRFromPod(ctx, clientset, namespace, svcCIDR)
		if err == nil {
			result = append(result, podCIDR...)
		}
	}

	return result
}

// ParseCIDRFromString
/*
*
kube-apiserver:
--service-cluster-ip-range=<IPv4 CIDR>,<IPv6 CIDR>
kube-controller-manager:
--cluster-cidr=<IPv4 CIDR>,<IPv6 CIDR>
--service-cluster-ip-range=<IPv4 CIDR>,<IPv6 CIDR>
--node-cidr-mask-size-ipv4|--node-cidr-mask-size-ipv6 defaults to /24 for IPv4 and /64 for IPv6
kube-proxy:
--cluster-cidr=<IPv4 CIDR>,<IPv6 CIDR>
*/
func ParseCIDRFromString(content string) (result []*net.IPNet) {
	if strings.Contains(content, "cluster-cidr") || strings.Contains(content, "service-cluster-ip-range") {
		split := strings.Split(content, "=")
		if len(split) == 2 {
			cidrList := split[1]
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

// GetCIDRByDumpClusterInfo
// root     22008 21846 14 Jan18 ?        6-22:53:35 kube-apiserver --advertise-address=10.56.95.185 --allow-privileged=true --anonymous-auth=True --apiserver-count=3 --authorization-mode=Node,RBAC --bind-address=0.0.0.0 --client-ca-file=/etc/kubernetes/ssl/ca.crt --default-not-ready-toleration-seconds=300 --default-unreachable-toleration-seconds=300 --enable-admission-plugins=NodeRestriction --enable-aggregator-routing=False --enable-bootstrap-token-auth=true --endpoint-reconciler-type=lease --etcd-cafile=/etc/ssl/etcd/ssl/ca.pem --etcd-certfile=/etc/ssl/etcd/ssl/node-kube-control-1.pem --etcd-keyfile=/etc/ssl/etcd/ssl/node-kube-control-1-key.pem --etcd-servers=https://10.56.95.185:2379,https://10.56.95.186:2379,https://10.56.95.187:2379 --etcd-servers-overrides=/events#https://10.56.95.185:2381;https://10.56.95.186:2381;https://10.56.95.187:2381 --event-ttl=1h0m0s --insecure-port=0 --kubelet-certificate-authority=/etc/kubernetes/ssl/kubelet/kubelet-ca.crt --kubelet-client-certificate=/etc/kubernetes/ssl/apiserver-kubelet-client.crt --kubelet-client-key=/etc/kubernetes/ssl/apiserver-kubelet-client.key --kubelet-preferred-address-types=InternalDNS,InternalIP,Hostname,ExternalDNS,ExternalIP --profiling=False --proxy-client-cert-file=/etc/kubernetes/ssl/front-proxy-client.crt --proxy-client-key-file=/etc/kubernetes/ssl/front-proxy-client.key --request-timeout=1m0s --requestheader-allowed-names=front-proxy-client --requestheader-client-ca-file=/etc/kubernetes/ssl/front-proxy-ca.crt --requestheader-extra-headers-prefix=X-Remote-Extra- --requestheader-group-headers=X-Remote-Group --requestheader-username-headers=X-Remote-User --secure-port=6443 --service-account-issuer=https://kubernetes.default.svc.cluster.local --service-account-key-file=/etc/kubernetes/ssl/sa.pub --service-account-signing-key-file=/etc/kubernetes/ssl/sa.key --service-cluster-ip-range=10.233.0.0/18 --service-node-port-range=30000-32767 --storage-backend=etcd3 --tls-cert-file=/etc/kubernetes/ssl/apiserver.crt --tls-private-key-file=/etc/kubernetes/ssl/apiserver.key
// ref: https://kubernetes.io/docs/concepts/services-networking/dual-stack/#configure-ipv4-ipv6-dual-stack
// get cidr by dump cluster info
func GetCIDRByDumpClusterInfo(ctx context.Context, clientset *kubernetes.Clientset) ([]*net.IPNet, error) {
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
		result = append(result, ParseCIDRFromString(s)...)
	}
	return result, nil
}

// GetCIDRFromCNI kube-controller-manager--allocate-node-cidrs=true--authentication-kubeconfig=/etc/kubernetes/controller-manager.conf--authorization-kubeconfig=/etc/kubernetes/controller-manager.conf--bind-address=0.0.0.0--client-ca-file=/etc/kubernetes/ssl/ca.crt--cluster-cidr=10.233.64.0/18--cluster-name=cluster.local--cluster-signing-cert-file=/etc/kubernetes/ssl/ca.crt--cluster-signing-key-file=/etc/kubernetes/ssl/ca.key--configure-cloud-routes=false--controllers=*,bootstrapsigner,tokencleaner--kubeconfig=/etc/kubernetes/controller-manager.conf--leader-elect=true--leader-elect-lease-duration=15s--leader-elect-renew-deadline=10s--node-cidr-mask-size=24--node-monitor-grace-period=40s--node-monitor-period=5s--port=0--profiling=False--requestheader-client-ca-file=/etc/kubernetes/ssl/front-proxy-ca.crt--root-ca-file=/etc/kubernetes/ssl/ca.crt--service-account-private-key-file=/etc/kubernetes/ssl/sa.key--service-cluster-ip-range=10.233.0.0/18--terminated-pod-gc-threshold=12500--use-service-account-credentials=true
func GetCIDRFromCNI(ctx context.Context, clientset *kubernetes.Clientset, restconfig *rest.Config, namespace string, image string) ([]*net.IPNet, error) {
	pod, err := CreateCIDRPod(ctx, clientset, namespace, image)
	if err != nil {
		return nil, err
	}

	var cmd = `grep -a -R "service-cluster-ip-range\|cluster-cidr" /etc/cni/proc/*/cmdline | grep -a -v grep | tr "\0" "\n"`

	var content string
	content, err = Shell(ctx, clientset, restconfig, pod.Name, "", pod.Namespace, []string{"sh", "-c", cmd})
	if err != nil {
		return nil, err
	}

	var result []*net.IPNet
	for _, s := range strings.Split(content, "\n") {
		result = append(result, ParseCIDRFromString(s)...)
	}

	return result, nil
}

func GetServiceCIDRByCreateService(ctx context.Context, serviceInterface v12.ServiceInterface) (*net.IPNet, error) {
	defaultCIDRIndex := "valid IPs is"
	svc := &v13.Service{
		ObjectMeta: v1.ObjectMeta{GenerateName: "foo-svc-"},
		Spec:       v13.ServiceSpec{Ports: []v13.ServicePort{{Port: 80}}, ClusterIP: "0.0.0.0"},
	}
	_, err := serviceInterface.Create(ctx, svc, v1.CreateOptions{})
	if err != nil {
		idx := strings.LastIndex(err.Error(), defaultCIDRIndex)
		if idx != -1 {
			_, cidr, err1 := net.ParseCIDR(strings.TrimSpace(err.Error()[idx+len(defaultCIDRIndex):]))
			return cidr, err1
		}
		return nil, fmt.Errorf("can not found any keyword of service network CIDR info: %s", err.Error())
	}
	return nil, fmt.Errorf("can not found any keyword of service network CIDR info")
}

// GetPodCIDRFromCNI
/*
*

	{
	  "name": "cni0",
	  "cniVersion":"0.3.1",
	  "plugins":[
	    {
	      "datastore_type": "kubernetes",
	      "nodename": "10.56.95.185",
	      "type": "calico",
	      "log_level": "info",
	      "log_file_path": "/var/log/calico/cni/cni.log",
	      "ipam": {
	        "type": "calico-ipam",
	        "assign_ipv4": "true",
	        "ipv4_pools": ["10.233.64.0/18"]
	      },
	      "policy": {
	        "type": "k8s"
	      },
	      "kubernetes": {
	        "kubeconfig": "/etc/cni/net.d/calico-kubeconfig"
	      }
	    },
	    {
	      "type":"portmap",
	      "capabilities": {
	        "portMappings": true
	      }
	    }
	  ]
	}
*/
func GetPodCIDRFromCNI(ctx context.Context, clientset *kubernetes.Clientset, restconfig *rest.Config, namespace string) ([]*net.IPNet, error) {
	//var cmd = "cat /etc/cni/net.d/*.conflist"
	content, err := Shell(ctx, clientset, restconfig, config.CniNetName, "", namespace, []string{"cat", "/etc/cni/net.d/*.conflist"})
	if err != nil {
		return nil, err
	}

	configList, err := libcni.ConfListFromBytes([]byte(content))
	if err != nil {
		return nil, err
	}
	plog.G(ctx).Infoln("Get CNI config", configList.Name)
	var cidrList []*net.IPNet
	for _, plugin := range configList.Plugins {
		switch plugin.Network.Type {
		case "calico":
			var m = map[string]interface{}{}
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

func CreateCIDRPod(ctx context.Context, clientset *kubernetes.Clientset, namespace string, image string) (*v13.Pod, error) {
	var procName = "proc-dir-kubevpn"
	pod := &v13.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      config.CniNetName,
			Namespace: namespace,
		},
		Spec: v13.PodSpec{
			Volumes: []v13.Volume{
				{
					Name: config.CniNetName,
					VolumeSource: v13.VolumeSource{
						HostPath: &v13.HostPathVolumeSource{
							Path: config.DefaultNetDir,
							Type: ptr.To[v13.HostPathType](v13.HostPathDirectoryOrCreate),
						},
					},
				},
				{
					Name: procName,
					VolumeSource: v13.VolumeSource{
						HostPath: &v13.HostPathVolumeSource{
							Path: config.Proc,
							Type: ptr.To[v13.HostPathType](v13.HostPathDirectoryOrCreate),
						},
					},
				},
			},
			Containers: []v13.Container{
				{
					Name:    config.CniNetName,
					Image:   image,
					Command: []string{"tail", "-f", "/dev/null"},
					Resources: v13.ResourceRequirements{
						Requests: map[v13.ResourceName]resource.Quantity{
							v13.ResourceCPU:    resource.MustParse("16m"),
							v13.ResourceMemory: resource.MustParse("16Mi"),
						},
						Limits: map[v13.ResourceName]resource.Quantity{
							v13.ResourceCPU:    resource.MustParse("16m"),
							v13.ResourceMemory: resource.MustParse("16Mi"),
						},
					},
					VolumeMounts: []v13.VolumeMount{
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
					ImagePullPolicy: v13.PullIfNotPresent,
				},
			},
			Affinity: &v13.Affinity{
				NodeAffinity: &v13.NodeAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []v13.PreferredSchedulingTerm{
						{
							Weight: 50,
							Preference: v13.NodeSelectorTerm{
								MatchExpressions: []v13.NodeSelectorRequirement{
									{
										Key:      "node-role.kubernetes.io/master",
										Operator: v13.NodeSelectorOpExists,
									},
									{
										Key:      "node-role.kubernetes.io/control-plane",
										Operator: v13.NodeSelectorOpExists,
									},
								},
							},
						},
					},
				},
			},
			Tolerations: []v13.Toleration{
				{
					Key:      "node-role.kubernetes.io/master",
					Operator: v13.TolerationOpEqual,
					Effect:   v13.TaintEffectNoSchedule,
				}, {
					Key:      "node-role.kubernetes.io/control-plane",
					Operator: v13.TolerationOpEqual,
					Effect:   v13.TaintEffectNoSchedule,
				},
			},
			TopologySpreadConstraints: []v13.TopologySpreadConstraint{
				{
					MaxSkew:           1,
					TopologyKey:       "kubernetes.io/hostname",
					WhenUnsatisfiable: v13.ScheduleAnyway,
					LabelSelector:     v1.SetAsLabelSelector(map[string]string{"app": config.ConfigMapPodTrafficManager}),
				},
			},
		},
	}
	get, err := clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, v1.GetOptions{})
	if errors.IsNotFound(err) || get.Status.Phase != v13.PodRunning {
		if get.Status.Phase != v13.PodRunning {
			_ = clientset.CoreV1().Pods(namespace).Delete(context.Background(), pod.Name, v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)})
		}
		pod, err = clientset.CoreV1().Pods(namespace).Create(ctx, pod, v1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		checker := func(pod *v13.Pod) bool {
			isRunning := pod.Status.Phase == v13.PodRunning
			return isRunning
		}
		field := fields.OneTermEqualSelector("metadata.name", pod.Name).String()
		ctx2, cancelFunc := context.WithTimeout(ctx, time.Second*15)
		defer cancelFunc()
		err = WaitPod(ctx2, clientset.CoreV1().Pods(namespace), v1.ListOptions{FieldSelector: field}, checker)
		if err != nil {
			return nil, err
		}
	}
	return pod, nil
}

func GetPodCIDRFromPod(ctx context.Context, clientset *kubernetes.Clientset, namespace string, svc *net.IPNet) ([]*net.IPNet, error) {
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
				_, ipNet, _ := net.ParseCIDR((&net.IPNet{IP: ip, Mask: svc.Mask}).String())
				if ipNet != nil {
					result = append(result, ipNet)
				}
			}
		}
	}

	return result, nil
}

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
	var set = sets.New[string]()
	if ip := net.ParseIP(host); ip != nil {
		if !set.Has(ip.String()) {
			ipList = append(ipList, ip)
			set.Insert(ip.String())
		}
	}

	addrs, _ := net.LookupHost(host)
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil {
			continue
		}
		if !set.Has(ip.String()) {
			ipList = append(ipList, ip)
			set.Insert(ip.String())
		}
	}
	return ipList, nil
}

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
