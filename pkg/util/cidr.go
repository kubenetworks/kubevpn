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

func GetCIDRFromResource(clientset *kubernetes.Clientset, namespace string) ([]*net.IPNet, error) {
	var list []*net.IPNet
	// (1) get pod CIDR from node spec
	nodeList, err := clientset.CoreV1().Nodes().List(context.TODO(), v1.ListOptions{})
	for _, node := range nodeList.Items {
		for _, c := range sets.NewString(node.Spec.PodCIDRs...).Insert(node.Spec.PodCIDR).List() {
			_, cidr, _ := net.ParseCIDR(c)
			if cidr != nil {
				list = append(list, cidr)
			}
		}
	}

	// (2) get pod CIDR from pod ip, why doing this: notice that node's pod cidr is not correct in minikube
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
	podList, _ := clientset.CoreV1().Pods(namespace).List(context.TODO(), v1.ListOptions{})
	for _, pod := range podList.Items {
		if pod.Spec.HostNetwork {
			continue
		}
		if ip := net.ParseIP(pod.Status.PodIP); ip != nil {
			var contain bool
			for _, cidr := range list {
				if cidr.Contains(ip) {
					contain = true
					break
				}
			}
			if !contain {
				mask := net.CIDRMask(24, 32)
				list = append(list, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
			}
		}
	}

	// (3) get service CIDR
	defaultCIDRIndex := "valid IPs is"
	_, err = clientset.CoreV1().Services(namespace).Create(context.TODO(), &v12.Service{
		ObjectMeta: v1.ObjectMeta{GenerateName: "foo-svc-"},
		Spec:       v12.ServiceSpec{Ports: []v12.ServicePort{{Port: 80}}, ClusterIP: "0.0.0.0"},
	}, v1.CreateOptions{})
	if err != nil {
		idx := strings.LastIndex(err.Error(), defaultCIDRIndex)
		if idx != -1 {
			_, cidr, _ := net.ParseCIDR(strings.TrimSpace(err.Error()[idx+len(defaultCIDRIndex):]))
			if cidr != nil {
				list = append(list, cidr)
			}
		}
	} else {
		serviceList, _ := clientset.CoreV1().Services(namespace).List(context.TODO(), v1.ListOptions{})
		for _, service := range serviceList.Items {
			if ip := net.ParseIP(service.Spec.ClusterIP); ip != nil {
				var contain bool
				for _, CIDR := range list {
					if CIDR.Contains(ip) {
						contain = true
						break
					}
				}
				if !contain {
					mask := net.CIDRMask(24, 32)
					list = append(list, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
				}
			}
		}
	}

	// (4) remove duplicate CIDR
	result := make([]*net.IPNet, 0)
	set := sets.NewString()
	for _, cidr := range list {
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

func GetCidrFromCNI(clientset *kubernetes.Clientset, restclient *rest.RESTClient, restconfig *rest.Config, namespace string) ([]*net.IPNet, error) {
	var name = "cni-net-dir-kubevpn"
	var procName = "proc-dir-kubevpn"
	hostPathType := v12.HostPathDirectoryOrCreate
	pod := &v12.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v12.PodSpec{
			Volumes: []v12.Volume{
				{
					Name: name,
					VolumeSource: v12.VolumeSource{
						HostPath: &v12.HostPathVolumeSource{
							Path: config.DefaultNetDir,
							Type: &hostPathType,
						},
					},
				},
				{
					Name: procName,
					VolumeSource: v12.VolumeSource{
						HostPath: &v12.HostPathVolumeSource{
							Path: config.Proc,
							Type: &hostPathType,
						},
					},
				},
			},
			Containers: []v12.Container{
				{
					Name:    name,
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
							Name:      name,
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
		},
	}
	get, err := clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, v1.GetOptions{})
	if k8serrors.IsNotFound(err) || get.Status.Phase != v12.PodRunning {
		var deleteFunc = func() {
			_ = clientset.CoreV1().Pods(namespace).Delete(context.Background(), pod.Name, v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)})
		}
		// delete pod anyway
		deleteFunc()
		pod, err = clientset.CoreV1().Pods(namespace).Create(context.Background(), pod, v1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		defer deleteFunc()
		err = WaitPod(clientset.CoreV1().Pods(namespace), v1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("metadata.name", pod.Name).String(),
		}, func(pod *v12.Pod) bool {
			return pod.Status.Phase == v12.PodRunning
		})
		if err != nil {
			return nil, err
		}
	}

	var cmds = []string{
		"cat /etc/cni/net.d/*.conflist",
		`grep -a -R "service-cluster-ip-range\|cluster-ip-range\|cluster-cidr\|cidr" /etc/cni/proc/*/cmdline | grep -a -v grep`,
	}

	var result []*net.IPNet

	for _, cmd := range cmds {
		content, err := Shell(clientset, restclient, restconfig, pod.Name, pod.Namespace, cmd)
		if err != nil {
			return nil, err
		}

		conf, err := libcni.ConfListFromFile(content)
		if err == nil {
			log.Print("get cni {} config", conf.Name)
		}

		v4 := regexp.MustCompile(`(([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,})`)
		v6 := regexp.MustCompile(`(([0-9a-fA-F]{1,4}:){7,7}[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,7}:|([0-9a-fA-F]{1,4}:){1,6}:[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,5}(:[0-9a-fA-F]{1,4}){1,2}|([0-9a-fA-F]{1,4}:){1,4}(:[0-9a-fA-F]{1,4}){1,3}|([0-9a-fA-F]{1,4}:){1,3}(:[0-9a-fA-F]{1,4}){1,4}|([0-9a-fA-F]{1,4}:){1,2}(:[0-9a-fA-F]{1,4}){1,5}|[0-9a-fA-F]{1,4}:((:[0-9a-fA-F]{1,4}){1,6})|:((:[0-9a-fA-F]{1,4}){1,7}|:)|fe80:(:[0-9a-fA-F]{0,4}){0,4}%[0-9a-zA-Z]{1,}|::(ffff(:0{1,4}){0,1}:){0,1}((25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])|([0-9a-fA-F]{1,4}:){1,4}:((25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9]))/[0-9]{1,}`)

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
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("can not found any cidr")
	}

	return result, nil
}
