package util

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/containernetworking/cni/libcni"
	log "github.com/sirupsen/logrus"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// root     22008 21846 14 Jan18 ?        6-22:53:35 kube-apiserver --advertise-address=10.56.95.185 --allow-privileged=true --anonymous-auth=True --apiserver-count=3 --authorization-mode=Node,RBAC --bind-address=0.0.0.0 --client-ca-file=/etc/kubernetes/ssl/ca.crt --default-not-ready-toleration-seconds=300 --default-unreachable-toleration-seconds=300 --enable-admission-plugins=NodeRestriction --enable-aggregator-routing=False --enable-bootstrap-token-auth=true --endpoint-reconciler-type=lease --etcd-cafile=/etc/ssl/etcd/ssl/ca.pem --etcd-certfile=/etc/ssl/etcd/ssl/node-kube-control-1.pem --etcd-keyfile=/etc/ssl/etcd/ssl/node-kube-control-1-key.pem --etcd-servers=https://10.56.95.185:2379,https://10.56.95.186:2379,https://10.56.95.187:2379 --etcd-servers-overrides=/events#https://10.56.95.185:2381;https://10.56.95.186:2381;https://10.56.95.187:2381 --event-ttl=1h0m0s --insecure-port=0 --kubelet-certificate-authority=/etc/kubernetes/ssl/kubelet/kubelet-ca.crt --kubelet-client-certificate=/etc/kubernetes/ssl/apiserver-kubelet-client.crt --kubelet-client-key=/etc/kubernetes/ssl/apiserver-kubelet-client.key --kubelet-preferred-address-types=InternalDNS,InternalIP,Hostname,ExternalDNS,ExternalIP --profiling=False --proxy-client-cert-file=/etc/kubernetes/ssl/front-proxy-client.crt --proxy-client-key-file=/etc/kubernetes/ssl/front-proxy-client.key --request-timeout=1m0s --requestheader-allowed-names=front-proxy-client --requestheader-client-ca-file=/etc/kubernetes/ssl/front-proxy-ca.crt --requestheader-extra-headers-prefix=X-Remote-Extra- --requestheader-group-headers=X-Remote-Group --requestheader-username-headers=X-Remote-User --secure-port=6443 --service-account-issuer=https://kubernetes.default.svc.cluster.local --service-account-key-file=/etc/kubernetes/ssl/sa.pub --service-account-signing-key-file=/etc/kubernetes/ssl/sa.key --service-cluster-ip-range=10.233.0.0/18 --service-node-port-range=30000-32767 --storage-backend=etcd3 --tls-cert-file=/etc/kubernetes/ssl/apiserver.crt --tls-private-key-file=/etc/kubernetes/ssl/apiserver.key
// ref: https://kubernetes.io/docs/concepts/services-networking/dual-stack/#configure-ipv4-ipv6-dual-stack
// get cidr by dump cluster info
func getCIDRByDumpClusterInfo(clientset *kubernetes.Clientset) ([]*net.IPNet, error) {
	podList, err := clientset.CoreV1().Pods(v1.NamespaceSystem).List(context.Background(), v1.ListOptions{})
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
		result = append(result, parseCIDRFromString(s)...)
	}
	return Deduplicate(result), nil
}

// kube-controller-manager--allocate-node-cidrs=true--authentication-kubeconfig=/etc/kubernetes/controller-manager.conf--authorization-kubeconfig=/etc/kubernetes/controller-manager.conf--bind-address=0.0.0.0--client-ca-file=/etc/kubernetes/ssl/ca.crt--cluster-cidr=10.233.64.0/18--cluster-name=cluster.local--cluster-signing-cert-file=/etc/kubernetes/ssl/ca.crt--cluster-signing-key-file=/etc/kubernetes/ssl/ca.key--configure-cloud-routes=false--controllers=*,bootstrapsigner,tokencleaner--kubeconfig=/etc/kubernetes/controller-manager.conf--leader-elect=true--leader-elect-lease-duration=15s--leader-elect-renew-deadline=10s--node-cidr-mask-size=24--node-monitor-grace-period=40s--node-monitor-period=5s--port=0--profiling=False--requestheader-client-ca-file=/etc/kubernetes/ssl/front-proxy-ca.crt--root-ca-file=/etc/kubernetes/ssl/ca.crt--service-account-private-key-file=/etc/kubernetes/ssl/sa.key--service-cluster-ip-range=10.233.0.0/18--terminated-pod-gc-threshold=12500--use-service-account-credentials=true
func getCIDRFromCNI(clientset *kubernetes.Clientset, restclient *rest.RESTClient, restconfig *rest.Config, namespace string) ([]*net.IPNet, error) {
	pod, err := createCIDRPod(clientset, namespace)
	if err != nil {
		return nil, err
	}

	var cmd = `grep -a -R "service-cluster-ip-range\|cluster-cidr" /etc/cni/proc/*/cmdline | grep -a -v grep | tr "\0" "\n"`

	var content string
	content, err = Shell(clientset, restclient, restconfig, pod.Name, "", pod.Namespace, []string{"sh", "-c", cmd})
	if err != nil {
		return nil, err
	}

	var result []*net.IPNet
	for _, s := range strings.Split(content, "\n") {
		result = Deduplicate(append(result, parseCIDRFromString(s)...))
	}

	return result, nil
}

func getServiceCIDRByCreateSvc(serviceInterface corev1.ServiceInterface) (*net.IPNet, error) {
	defaultCIDRIndex := "valid IPs is"
	_, err := serviceInterface.Create(context.Background(), &v12.Service{
		ObjectMeta: v1.ObjectMeta{GenerateName: "foo-svc-"},
		Spec:       v12.ServiceSpec{Ports: []v12.ServicePort{{Port: 80}}, ClusterIP: "0.0.0.0"},
	}, v1.CreateOptions{})
	if err != nil {
		idx := strings.LastIndex(err.Error(), defaultCIDRIndex)
		if idx != -1 {
			_, cidr, err := net.ParseCIDR(strings.TrimSpace(err.Error()[idx+len(defaultCIDRIndex):]))
			if err != nil {
				return nil, err
			}
			return cidr, nil
		}
		return nil, fmt.Errorf("can not found any keyword of service cidr info, err: %s", err.Error())
	}
	return nil, err
}

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
func getPodCIDRFromCNI(clientset *kubernetes.Clientset, restclient *rest.RESTClient, restconfig *rest.Config, namespace string) ([]*net.IPNet, error) {
	//var cmd = "cat /etc/cni/net.d/*.conflist"
	content, err := Shell(clientset, restclient, restconfig, config.CniNetName, "", namespace, []string{"cat", "/etc/cni/net.d/*.conflist"})
	if err != nil {
		return nil, err
	}

	configList, err := libcni.ConfListFromBytes([]byte(content))
	if err == nil {
		log.Infoln("get cni config", configList.Name)
	}
	var cidr []*net.IPNet
	for _, plugin := range configList.Plugins {
		switch plugin.Network.Type {
		case "calico":
			var m map[string]interface{}
			_ = json.Unmarshal(plugin.Bytes, &m)
			slice, _, _ := unstructured.NestedStringSlice(m, "ipam", "ipv4_pools")
			slice6, _, _ := unstructured.NestedStringSlice(m, "ipam", "ipv6_pools")
			for _, s := range sets.New[string]().Insert(slice...).Insert(slice6...).UnsortedList() {
				if _, ipNet, _ := net.ParseCIDR(s); ipNet != nil {
					cidr = append(cidr, ipNet)
				}
			}
		}
	}

	return cidr, nil
}

func createCIDRPod(clientset *kubernetes.Clientset, namespace string) (*v12.Pod, error) {
	var procName = "proc-dir-kubevpn"
	pod := &v12.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      config.CniNetName,
			Namespace: namespace,
		},
		Spec: v12.PodSpec{
			Volumes: []v12.Volume{
				{
					Name: config.CniNetName,
					VolumeSource: v12.VolumeSource{
						HostPath: &v12.HostPathVolumeSource{
							Path: config.DefaultNetDir,
							Type: ptr.To[v12.HostPathType](v12.HostPathDirectoryOrCreate),
						},
					},
				},
				{
					Name: procName,
					VolumeSource: v12.VolumeSource{
						HostPath: &v12.HostPathVolumeSource{
							Path: config.Proc,
							Type: ptr.To[v12.HostPathType](v12.HostPathDirectoryOrCreate),
						},
					},
				},
			},
			Containers: []v12.Container{
				{
					Name:    config.CniNetName,
					Image:   config.Image,
					Command: []string{"tail", "-f", "/dev/null"},
					Resources: v12.ResourceRequirements{
						Requests: map[v12.ResourceName]resource.Quantity{
							v12.ResourceCPU:    resource.MustParse("16m"),
							v12.ResourceMemory: resource.MustParse("16Mi"),
						},
						Limits: map[v12.ResourceName]resource.Quantity{
							v12.ResourceCPU:    resource.MustParse("16m"),
							v12.ResourceMemory: resource.MustParse("16Mi"),
						},
					},
					VolumeMounts: []v12.VolumeMount{
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
					ImagePullPolicy: v12.PullIfNotPresent,
				},
			},
			Affinity: &v12.Affinity{
				NodeAffinity: &v12.NodeAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []v12.PreferredSchedulingTerm{
						{
							Weight: 50,
							Preference: v12.NodeSelectorTerm{
								MatchExpressions: []v12.NodeSelectorRequirement{
									{
										Key:      "node-role.kubernetes.io/master",
										Operator: v12.NodeSelectorOpExists,
									},
									{
										Key:      "node-role.kubernetes.io/control-plane",
										Operator: v12.NodeSelectorOpExists,
									},
								},
							},
						},
					},
				},
			},
			Tolerations: []v12.Toleration{
				{
					Key:      "node-role.kubernetes.io/master",
					Operator: v12.TolerationOpEqual,
					Effect:   v12.TaintEffectNoSchedule,
				}, {
					Key:      "node-role.kubernetes.io/control-plane",
					Operator: v12.TolerationOpEqual,
					Effect:   v12.TaintEffectNoSchedule,
				},
			},
			TopologySpreadConstraints: []v12.TopologySpreadConstraint{
				{
					MaxSkew:           1,
					TopologyKey:       "kubernetes.io/hostname",
					WhenUnsatisfiable: v12.ScheduleAnyway,
				},
			},
		},
	}
	get, err := clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, v1.GetOptions{})
	if errors.IsNotFound(err) || get.Status.Phase != v12.PodRunning {
		if get.Status.Phase != v12.PodRunning {
			_ = clientset.CoreV1().Pods(namespace).Delete(context.Background(), pod.Name, v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)})
		}
		pod, err = clientset.CoreV1().Pods(namespace).Create(context.Background(), pod, v1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		err = WaitPod(clientset.CoreV1().Pods(namespace), v1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("metadata.name", pod.Name).String(),
		}, func(pod *v12.Pod) bool {
			isRunning := pod.Status.Phase == v12.PodRunning
			if !isRunning {
				if message := PrintStatusInline(pod); message != "" {
					fmt.Printf("%s\r", message)
				}
			}
			return isRunning
		})
		if err != nil {
			fmt.Printf("wait pod %s to be running timeout, reason %s, ignore\r\n", pod.Name, pod.Status.Reason)
			return nil, err
		} else {
			fmt.Printf("\r")
		}
	}
	return pod, nil
}

func getPodCIDRFromPod(clientset *kubernetes.Clientset, namespace string, svc *net.IPNet) ([]*net.IPNet, error) {
	podList, err := clientset.CoreV1().Pods(namespace).List(context.Background(), v1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(podList.Items); i++ {
		if podList.Items[i].Spec.HostNetwork {
			podList.Items = append(podList.Items[:i], podList.Items[i+1:]...)
			i--
		}
	}
	var result []*net.IPNet
	for _, item := range podList.Items {
		s := sets.New[string]().Insert(item.Status.PodIP)
		for _, p := range item.Status.PodIPs {
			s.Insert(p.IP)
		}
		for _, t := range s.UnsortedList() {
			if ip := net.ParseIP(t); ip != nil {
				var mask net.IPMask
				if ip.To4() != nil {
					mask = net.CIDRMask(24, 32)
				} else {
					mask = net.CIDRMask(64, 128)
				}
				result = append(result, &net.IPNet{IP: ip, Mask: /*svc.Mask*/ mask})
			}
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("can not found pod cidr from pod list")
	}

	return result, nil

}

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
func parseCIDRFromString(content string) (result []*net.IPNet) {
	if strings.Contains(content, "cluster-cidr") || strings.Contains(content, "service-cluster-ip-range") {
		split := strings.Split(content, "=")
		if len(split) == 2 {
			cidrList := split[1]
			for _, cidr := range strings.Split(cidrList, ",") {
				_, c, err := net.ParseCIDR(cidr)
				if err == nil {
					result = append(result, c)
				}
			}
		}
	}
	return
}
