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
	"sort"
	"time"
)

const TrafficManager = "kubevpn.traffic.manager"

func CreateServer(clientset *kubernetes.Clientset, namespace, ip string) error {
	firstPod, i, err3 := polymorphichelpers.GetFirstPod(clientset.CoreV1(),
		namespace,
		fields.OneTermEqualSelector("app", TrafficManager).String(),
		time.Second*5,
		func(pods []*v1.Pod) sort.Interface {
			return sort.Reverse(podutils.ActivePods(pods))
		},
	)

	if err3 == nil && i != 0 && firstPod != nil {
		return nil
	}

	t := true
	zero := int64(0)
	name := TrafficManager
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": TrafficManager},
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
							"iptables -t nat -A POSTROUTING -s 192.168.254.0/24 -o eth0 -j MASQUERADE;" +
							"iptables -t nat -A POSTROUTING -s 172.20.0.0/16 -o eth0 -j MASQUERADE;" +
							"gost -L socks5://:10800 -L tun://:8421?net=" + ip + " -D",
					},
					SecurityContext: &v1.SecurityContext{
						Capabilities: &v1.Capabilities{
							Add: []v1.Capability{
								"NET_ADMIN",
								"SYS_MODULE",
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
					ImagePullPolicy: v1.PullIfNotPresent,
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
out:
	for {
		select {
		case e := <-watch.ResultChan():
			if e.Object.(*v1.Pod).Status.Phase == v1.PodRunning {
				watch.Stop()
				break out
			}
		case <-tick:
			watch.Stop()
			log.Error("timeout")
			return errors.New("timeout")
		}
	}
	return nil
}
