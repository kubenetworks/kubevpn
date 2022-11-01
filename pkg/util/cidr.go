package util

import (
	"context"
	"fmt"
	"github.com/containernetworking/cni/libcni"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/pkg/config"
	v12 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
	"net"
	"regexp"
	"strings"
)

func GetCIDR(clientset *kubernetes.Clientset, namespace string) ([]*net.IPNet, error) {
	var CIDRList []*net.IPNet
	// get pod CIDR from node spec
	nodeList, err := clientset.CoreV1().Nodes().List(context.TODO(), v1.ListOptions{})
	if err == nil {
		var podCIDRs = sets.NewString()
		for _, node := range nodeList.Items {
			if node.Spec.PodCIDRs != nil {
				podCIDRs.Insert(node.Spec.PodCIDRs...)
			}
			if len(node.Spec.PodCIDR) != 0 {
				podCIDRs.Insert(node.Spec.PodCIDR)
			}
		}
		for _, podCIDR := range podCIDRs.List() {
			if _, CIDR, err := net.ParseCIDR(podCIDR); err == nil {
				CIDRList = append(CIDRList, CIDR)
			}
		}
	}
	// get pod CIDR from pod ip, why doing this: notice that node's pod cidr is not correct in minikube
	// ➜  ~ kubectl get nodes -o jsonpath='{.items[*].spec.podCIDR}'
	//10.244.0.0/24%
	// ➜  ~  kubectl get pods -o=custom-columns=podIP:.status.podIP
	//podIP
	//172.17.0.5
	//172.17.0.4
	//172.17.0.4
	//172.17.0.3
	//172.17.0.3
	//172.17.0.6
	//172.17.0.8
	//172.17.0.3
	//172.17.0.7
	//172.17.0.2
	podList, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), v1.ListOptions{})
	if err == nil {
		for _, pod := range podList.Items {
			if pod.Spec.HostNetwork {
				continue
			}
			if ip := net.ParseIP(pod.Status.PodIP); ip != nil {
				var contain bool
				for _, CIDR := range CIDRList {
					if CIDR.Contains(ip) {
						contain = true
						break
					}
				}
				if !contain {
					mask := net.CIDRMask(24, 32)
					CIDRList = append(CIDRList, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
				}
			}
		}
	}

	// get service CIDR
	defaultCIDRIndex := "The range of valid IPs is"
	_, err = clientset.CoreV1().Services(namespace).Create(context.TODO(), &v12.Service{
		ObjectMeta: v1.ObjectMeta{GenerateName: "foo-svc-"},
		Spec:       v12.ServiceSpec{Ports: []v12.ServicePort{{Port: 80}}, ClusterIP: "0.0.0.0"},
	}, v1.CreateOptions{})
	if err != nil {
		idx := strings.LastIndex(err.Error(), defaultCIDRIndex)
		if idx != -1 {
			_, cidr, err := net.ParseCIDR(strings.TrimSpace(err.Error()[idx+len(defaultCIDRIndex):]))
			if err == nil {
				CIDRList = append(CIDRList, cidr)
			}
		}
	} else {
		serviceList, err := clientset.CoreV1().Services(namespace).List(context.TODO(), v1.ListOptions{})
		if err == nil {
			for _, service := range serviceList.Items {
				if ip := net.ParseIP(service.Spec.ClusterIP); ip != nil {
					var contain bool
					for _, CIDR := range CIDRList {
						if CIDR.Contains(ip) {
							contain = true
							break
						}
					}
					if !contain {
						mask := net.CIDRMask(16, 32)
						CIDRList = append(CIDRList, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
					}
				}
			}
		}
	}

	// remove duplicate CIDR
	result := make([]*net.IPNet, 0)
	set := sets.NewString()
	for _, cidr := range CIDRList {
		if !set.Has(cidr.String()) {
			set.Insert(cidr.String())
			result = append(result, cidr)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("can not found any CIDR")
	}
	return result, nil
}

// todo use patch to update this pod
func GetCidrFromCNI(clientset *kubernetes.Clientset, restclient *rest.RESTClient, restconfig *rest.Config, namespace string) ([]*net.IPNet, error) {
	hostPathType := v12.HostPathDirectoryOrCreate
	pod := &v12.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Spec: v12.PodSpec{
			Volumes: []v12.Volume{
				{
					Name: "cni-net-dir",
					VolumeSource: v12.VolumeSource{
						HostPath: &v12.HostPathVolumeSource{
							Path: config.DefaultNetDir,
							Type: &hostPathType,
						},
					},
				},
			},
			Containers: []v12.Container{
				{
					Name:    config.ContainerSidecarVPN,
					Image:   "guosen-dev.cargo.io/epscplibrary/kubevpn:latest",
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
							Name:      "cni-net-dir",
							ReadOnly:  true,
							MountPath: config.DefaultNetDir,
						},
					},
					ImagePullPolicy: v12.PullIfNotPresent,
				},
			},
		},
	}
	get, err := clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, v1.GetOptions{})
	if k8serrors.IsNotFound(err) || get.Status.Phase != v12.PodRunning {
		create, err := clientset.CoreV1().Pods(namespace).Create(context.Background(), pod, v1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		defer clientset.CoreV1().Pods(namespace).Delete(context.TODO(), create.Name, v1.DeleteOptions{
			GracePeriodSeconds: pointer.Int64(0),
		})
		err = WaitPod(clientset.CoreV1().Pods(namespace), v1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("metadata.name", create.Name).String(),
		}, func(pod *v12.Pod) bool {
			return pod.Status.Phase == v12.PodRunning
		})
		if err != nil {
			return nil, err
		}
	}
	content, err := Shell(clientset, restclient, restconfig, pod.Name, pod.Namespace, "cat /etc/cni/net.d/*.conflist")
	if err != nil {
		return nil, err
	}

	conf, err := libcni.ConfListFromFile(content)
	if err == nil {
		log.Print("get cni {} config", conf.Name)
	}

	v4 := regexp.MustCompile(`(([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,})`)
	v6 := regexp.MustCompile(`(([0-9a-fA-F]{1,4}:){7,7}[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,7}:|([0-9a-fA-F]{1,4}:){1,6}:[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,5}(:[0-9a-fA-F]{1,4}){1,2}|([0-9a-fA-F]{1,4}:){1,4}(:[0-9a-fA-F]{1,4}){1,3}|([0-9a-fA-F]{1,4}:){1,3}(:[0-9a-fA-F]{1,4}){1,4}|([0-9a-fA-F]{1,4}:){1,2}(:[0-9a-fA-F]{1,4}){1,5}|[0-9a-fA-F]{1,4}:((:[0-9a-fA-F]{1,4}){1,6})|:((:[0-9a-fA-F]{1,4}){1,7}|:)|fe80:(:[0-9a-fA-F]{0,4}){0,4}%[0-9a-zA-Z]{1,}|::(ffff(:0{1,4}){0,1}:){0,1}((25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])|([0-9a-fA-F]{1,4}:){1,4}:((25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9]))/[0-9]{1,}`)

	var result []*net.IPNet
	ipv4 := v4.FindAllString(content, -1)
	ipv6 := v6.FindAllString(content, -1)

	for _, s := range ipv4 {
		_, ipNet, err := net.ParseCIDR(s)
		if err == nil {
			result = append(result, ipNet)
		}
	}

	for _, s := range ipv6 {
		_, ipNet, err := net.ParseCIDR(s)
		if err == nil {
			result = append(result, ipNet)
		}
	}

	return result, nil
}
