package pkg

import (
	"context"
	"errors"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/pkg/exchange"
	"github.com/wencaiwulue/kubevpn/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/podutils"
	"net"
	"sort"
	"strings"
	"time"
)

func CreateOutboundRouterPod(clientset *kubernetes.Clientset, namespace string, trafficManagerIP *net.IPNet, nodeCIDR []*net.IPNet) (net.IP, error) {
	firstPod, i, err3 := polymorphichelpers.GetFirstPod(clientset.CoreV1(),
		namespace,
		fields.OneTermEqualSelector("app", util.TrafficManager).String(),
		time.Second*5,
		func(pods []*v1.Pod) sort.Interface {
			return sort.Reverse(podutils.ActivePods(pods))
		},
	)

	if err3 == nil && i != 0 && firstPod != nil {
		UpdateRefCount(clientset, namespace, firstPod.Name, 1)
		return net.ParseIP(firstPod.Status.PodIP), nil
	}
	args := []string{
		"sysctl net.ipv4.ip_forward=1",
		"iptables -F",
		"iptables -P INPUT ACCEPT",
		"iptables -P FORWARD ACCEPT",
		"iptables -t nat -A POSTROUTING -s 223.254.254.0/24 -o eth0 -j MASQUERADE",
	}
	for _, ipNet := range nodeCIDR {
		args = append(args, "iptables -t nat -A POSTROUTING -s "+ipNet.String()+" -o eth0 -j MASQUERADE")
	}
	args = append(args, "kubevpn serve -L tcp://:10800 -L tun://:8421?net="+trafficManagerIP.String()+" --debug=true")

	t := true
	zero := int64(0)
	name := util.TrafficManager
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      map[string]string{"app": util.TrafficManager},
			Annotations: map[string]string{"ref-count": "1"},
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyAlways,
			Containers: []v1.Container{
				{
					Name:    "vpn",
					Image:   "naison/kubevpn:v2",
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
				return net.ParseIP(e.Object.(*v1.Pod).Status.PodIP), nil
			}
		case <-tick:
			watch.Stop()
			log.Error("timeout")
			return nil, errors.New("timeout")
		}
	}
}

func CreateInboundPod(factory cmdutil.Factory, clientset *kubernetes.Clientset, namespace, workloads string, config PodRouteConfig) error {
	resourceTuple, parsed, err2 := util.SplitResourceTypeName(workloads)
	if !parsed || err2 != nil {
		return errors.New("not need")
	}
	newName := resourceTuple.Name + "-" + "shadow"
	util.DeletePod(clientset, namespace, newName)
	var sc exchange.Scalable
	switch strings.ToLower(resourceTuple.Resource) {
	case "deployment", "deployments":
		sc = exchange.NewDeploymentController(factory, clientset, namespace, resourceTuple.Name)
	case "statefulset", "statefulsets":
		sc = exchange.NewStatefulsetController(factory, clientset, namespace, resourceTuple.Name)
	case "replicaset", "replicasets":
		sc = exchange.NewReplicasController(factory, clientset, namespace, resourceTuple.Name)
	case "service", "services":
		sc = exchange.NewServiceController(factory, clientset, namespace, resourceTuple.Name)
	case "pod", "pods":
		sc = exchange.NewPodController(factory, clientset, namespace, "pods", resourceTuple.Name)
	default:
		sc = exchange.NewPodController(factory, clientset, namespace, resourceTuple.Resource, resourceTuple.Name)
	}
	rollbackFuncs = append(rollbackFuncs, func() {
		if err := sc.Cancel(); err != nil {
			log.Warnln(err)
		}
	})
	labels, ports, err2 := sc.ScaleToZero()
	//sc.CreateOutboundPod()
	return createInboundPod(newName, namespace, labels, ports, clientset, config)
}

func createInboundPod(newName string,
	namespace string,
	labels map[string]string,
	ports []v1.ContainerPort,
	clientset *kubernetes.Clientset,
	c PodRouteConfig,
) error {
	t := true
	zero := int64(0)
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newName,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyAlways,
			Containers: []v1.Container{
				{
					Name:    "vpn",
					Image:   "naison/kubevpn:v2",
					Command: []string{"/bin/sh", "-c"},
					Args: []string{
						"sysctl net.ipv4.ip_forward=1;" +
							"iptables -F;" +
							"iptables -P INPUT ACCEPT;" +
							"iptables -P FORWARD ACCEPT;" +
							"iptables -t nat -A PREROUTING -i eth0 -p tcp --dport 80:60000 -j DNAT --to " + c.LocalTunIP + ":80-60000;" +
							"iptables -t nat -A POSTROUTING -p tcp -m tcp --dport 80:60000 -j MASQUERADE;" +
							"iptables -t nat -A PREROUTING -i eth0 -p udp --dport 80:60000 -j DNAT --to " + c.LocalTunIP + ":80-60000;" +
							"iptables -t nat -A POSTROUTING -p udp -m udp --dport 80:60000 -j MASQUERADE;" +
							"kubevpn serve -L 'tun://0.0.0.0:8421/" + c.TrafficManagerRealIP + ":8421?net=" + c.InboundPodTunIP + "&route=" + c.Route + "' --debug=true",
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
					// without helm, not set ports are works fine, but if using helm, must set this filed, otherwise
					// this pod will not add to service's endpoint
					Ports: ports,
				},
			},
			PriorityClassName: "system-cluster-critical",
		},
	}
	if _, err := clientset.CoreV1().Pods(namespace).Create(context.TODO(), &pod, metav1.CreateOptions{}); err != nil {
		log.Fatal(err)
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
