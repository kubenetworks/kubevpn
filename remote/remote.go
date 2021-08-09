package remote

import (
	"context"
	"errors"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/podutils"
	"kubevpn/util"
	"net"
	"sort"
	"strings"
	"time"
)

func CreateServerOutbound(clientset *kubernetes.Clientset, namespace string, serverIp *net.IPNet, nodeCIDR []*net.IPNet) (*v1.Pod, error) {
	firstPod, i, err3 := polymorphichelpers.GetFirstPod(clientset.CoreV1(),
		namespace,
		fields.OneTermEqualSelector("app", util.TrafficManager).String(),
		time.Second*5,
		func(pods []*v1.Pod) sort.Interface {
			return sort.Reverse(podutils.ActivePods(pods))
		},
	)

	if err3 == nil && i != 0 && firstPod != nil {
		return firstPod, nil
	}
	args := []string{
		"sysctl net.ipv4.ip_forward=1",
		"iptables -F",
		"iptables -P INPUT ACCEPT",
		"iptables -P FORWARD ACCEPT",
		"iptables -t nat -A POSTROUTING -s 192.168.254.0/24 -o eth0 -j MASQUERADE",
	}
	for _, ipNet := range nodeCIDR {
		args = append(args, "iptables -t nat -A POSTROUTING -s "+ipNet.String()+" -o eth0 -j MASQUERADE")
	}
	args = append(args, "gost -L socks5://:10800 -L tun://:8421?net="+serverIp.String()+" -D")

	t := true
	zero := int64(0)
	name := util.TrafficManager
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": util.TrafficManager},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    "vpn",
					Image:   "naison/kubevpn:latest",
					Command: []string{"/bin/sh", "-c"},
					Args:    []string{strings.Join(args, ";")},
					SecurityContext: &v1.SecurityContext{
						Capabilities: &v1.Capabilities{
							Add: []v1.Capability{
								"NET_ADMIN",
								//"SYS_MODULE",
							},
						},
						RunAsUser:  &zero,
						Privileged: &t,
					},
					Resources: v1.ResourceRequirements{
						Requests: map[v1.ResourceName]resource.Quantity{
							v1.ResourceCPU:    resource.MustParse("128m"),
							v1.ResourceMemory: resource.MustParse("256Mi"),
						},
						Limits: map[v1.ResourceName]resource.Quantity{
							v1.ResourceCPU:    resource.MustParse("256m"),
							v1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
					ImagePullPolicy: v1.PullAlways,
				},
			},
			PriorityClassName: "system-cluster-critical",
		},
	}
	_, err2 := clientset.CoreV1().Pods(namespace).Create(context.TODO(), &pod, metav1.CreateOptions{})
	if err2 != nil {
		log.Fatal(err2)
	}
	watch, err := clientset.CoreV1().Pods(namespace).Watch(context.TODO(), metav1.SingleObject(metav1.ObjectMeta{Name: name}))
	if err != nil {
		log.Fatal(err)
	}
	tick := time.Tick(time.Minute * 2)
	for {
		select {
		case e := <-watch.ResultChan():
			if e.Object.(*v1.Pod).Status.Phase == v1.PodRunning {
				watch.Stop()
				return e.Object.(*v1.Pod), nil
			}
		case <-tick:
			watch.Stop()
			log.Error("timeout")
			return nil, errors.New("timeout")
		}
	}
}

func CreateServerOutboundAndInbound(clientset *kubernetes.Clientset, namespace, service string, virtualLocalIp, realRouterIP, virtualShadowIp string) error {
	lables := updateReplicasToZeroAndGetLabels(clientset, namespace, service)
	newName := service + "-" + "shadow"
	t := true
	zero := int64(0)
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newName,
			Namespace: namespace,
			Labels:    lables,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    "vpn",
					Image:   "naison/kubevpn:latest",
					Command: []string{"/bin/sh", "-c"},
					Args: []string{
						"sysctl net.ipv4.ip_forward=1;" +
							"iptables -F;" +
							"iptables -P INPUT ACCEPT;" +
							"iptables -P FORWARD ACCEPT;" +
							"iptables -t nat -A PREROUTING -i eth0 -p tcp --dport 2000:60000 -j DNAT --to " + virtualLocalIp + ":2000-60000;" +
							"iptables -t nat -A POSTROUTING -p tcp -m tcp --dport 2000:60000 -j MASQUERADE;" +
							"iptables -t nat -A PREROUTING -i eth0 -p udp --dport 2000:60000 -j DNAT --to " + virtualLocalIp + ":2000-60000;" +
							"iptables -t nat -A POSTROUTING -p udp -m udp --dport 2000:60000 -j MASQUERADE;" +
							"gost -L 'tun://0.0.0.0:8421/127.0.0.1:8421?net=" + virtualShadowIp + "&route=172.20.0.0/16' -F 'socks5://" + realRouterIP + ":10800?notls=true'",
					},
					SecurityContext: &v1.SecurityContext{
						Capabilities: &v1.Capabilities{
							Add: []v1.Capability{
								"NET_ADMIN",
								//"SYS_MODULE",
							},
						},
						RunAsUser:  &zero,
						Privileged: &t,
					},
					Resources: v1.ResourceRequirements{
						Requests: map[v1.ResourceName]resource.Quantity{
							v1.ResourceCPU:    resource.MustParse("128m"),
							v1.ResourceMemory: resource.MustParse("128Mi"),
						},
						Limits: map[v1.ResourceName]resource.Quantity{
							v1.ResourceCPU:    resource.MustParse("256m"),
							v1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
					ImagePullPolicy: v1.PullAlways,
				},
			},
			PriorityClassName: "system-cluster-critical",
		},
	}
	_, err2 := clientset.CoreV1().Pods(namespace).Create(context.TODO(), &pod, metav1.CreateOptions{})
	if err2 != nil {
		log.Fatal(err2)
	}
	watch, err := clientset.CoreV1().Pods(namespace).Watch(context.TODO(), metav1.SingleObject(metav1.ObjectMeta{Name: newName}))
	if err != nil {
		log.Fatal(err)
	}
	tick := time.Tick(time.Minute * 2)
	for {
		select {
		case e := <-watch.ResultChan():
			if e.Object.(*v1.Pod).Status.Phase == v1.PodRunning {
				watch.Stop()
				return nil
			}
		case <-tick:
			watch.Stop()
			log.Error("timeout")
			return errors.New("timeout")
		}
	}
}

func updateReplicasToZeroAndGetLabels(clientset *kubernetes.Clientset, namespace, service string) map[string]string {
	if service == "" || namespace == "" {
		log.Info("no need to expose local service to remote")
		return nil
	}
	log.Info("prepare to expose local service to remote service: " + service)
	util.ScaleDeploymentReplicasTo(clientset, namespace, service, 0)
	labels := getLabels(clientset, namespace, service)
	if labels == nil {
		log.Info("fail to create shadow")
		return nil
	}
	return labels
}
func getLabels(clientset *kubernetes.Clientset, namespace, service string) map[string]string {
	get, err := clientset.CoreV1().Services(namespace).
		Get(context.TODO(), service, metav1.GetOptions{})
	if err != nil {
		log.Error(err)
		return nil
	}
	selector := get.Spec.Selector
	_, err = clientset.AppsV1().Deployments(namespace).Get(context.TODO(), service, metav1.GetOptions{})
	if err != nil {
		log.Error(err)
		return nil
	}
	newName := service + "-" + "shadow"
	deletePod(clientset, namespace, newName)
	return selector
}
