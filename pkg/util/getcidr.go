package util

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/containernetworking/cni/libcni"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/pkg/config"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/yaml"
)

// get cidr by dump cluster info
func getCIDRByDumpClusterInfo(clientset *kubernetes.Clientset) (result []*net.IPNet, err error) {
	p, err := clientset.CoreV1().Pods("kube-system").List(context.Background(), v1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("status.phase", string(v12.PodRunning)).String(),
	})
	if err != nil {
		return nil, err
	}
	marshal, err := yaml.Marshal(p)
	if err != nil {
		return nil, err
	}

	svcCIDR := `service-cluster-ip-range`
	podCIDR := `cluster-cidr`
	reader := bufio.NewReader(bytes.NewBufferString(string(marshal)))
	svc := sets.NewString()
	v4P := regexp.MustCompile(v4)
	v6P := regexp.MustCompile(v6)
	for {
		line, _, err := reader.ReadLine()
		if err != nil {
			break
		}
		if strings.Contains(string(line), svcCIDR) {
			ipv4 := v4P.FindAllString(string(line), -1)
			ipv6 := v6P.FindAllString(string(line), -1)
			svc.Insert(ipv4...).Insert(ipv6...)
		}
		if strings.Contains(string(line), podCIDR) {
			ipv4 := v4P.FindAllString(string(line), -1)
			ipv6 := v6P.FindAllString(string(line), -1)
			svc.Insert(ipv4...).Insert(ipv6...)
		}
	}

	for _, s := range svc.List() {
		_, ipnet, err := net.ParseCIDR(s)
		if err != nil {
			result = append(result, ipnet)
		}
	}

	return result, nil
}

func getCIDRFromCNI(clientset *kubernetes.Clientset, restclient *rest.RESTClient, restconfig *rest.Config, namespace string) ([]*net.IPNet, error) {
	pod, err := createCIDRPod(clientset, namespace)
	if err != nil {
		return nil, err
	}

	var cmd = `grep -a -R "service-cluster-ip-range\|cluster-ip-range\|cluster-cidr\|cidr" /etc/cni/proc/*/cmdline | grep -a -v grep`

	var result []*net.IPNet
	content, err := Shell(clientset, restclient, restconfig, pod.Name, pod.Namespace, cmd)
	if err != nil {
		return nil, err
	}
	result = parseCIDRFromString(content)

	if len(result) == 0 {
		return nil, fmt.Errorf("can not found any cidr")
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

func getPodCIDRFromCNI(clientset *kubernetes.Clientset, restclient *rest.RESTClient, restconfig *rest.Config, namespace string) ([]*net.IPNet, error) {
	pod, err := createCIDRPod(clientset, namespace)
	if err != nil {
		return nil, err
	}

	var cmd = "cat /etc/cni/net.d/*.conflist"

	content, err := Shell(clientset, restclient, restconfig, pod.Name, pod.Namespace, cmd)
	if err != nil {
		return nil, err
	}

	conf, err := libcni.ConfListFromFile(content)
	if err == nil {
		log.Infoln("get cni config", conf.Name)
	}

	result := parseCIDRFromString(content)

	if len(result) == 0 {
		return nil, fmt.Errorf("can not found any cidr")
	}

	return result, nil
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
							Type: (*v12.HostPathType)(pointer.String(string(v12.HostPathDirectoryOrCreate))),
						},
					},
				},
				{
					Name: procName,
					VolumeSource: v12.VolumeSource{
						HostPath: &v12.HostPathVolumeSource{
							Path: config.Proc,
							Type: (*v12.HostPathType)(pointer.String(string(v12.HostPathDirectoryOrCreate))),
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
		pod, err = clientset.CoreV1().Pods(namespace).Create(context.Background(), pod, v1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		err = WaitPod(clientset.CoreV1().Pods(namespace), v1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("metadata.name", pod.Name).String(),
		}, func(pod *v12.Pod) bool {
			return pod.Status.Phase == v12.PodRunning
		})
		if err != nil {
			return nil, err
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
		if podList.Items[i].Spec.HostNetwork || net.ParseIP(podList.Items[i].Status.PodIP) == nil {
			podList.Items = append(podList.Items[:i], podList.Items[i+1:]...)
			i--
		}
	}
	for _, item := range podList.Items {
		if item.Name == config.CniNetName {
			return []*net.IPNet{svc, {IP: net.ParseIP(item.Status.PodIP), Mask: svc.Mask}}, nil
		}
	}
	for _, item := range podList.Items {
		return []*net.IPNet{svc, {IP: net.ParseIP(item.Status.PodIP), Mask: svc.Mask}}, nil
	}
	return nil, fmt.Errorf("can not found pod cidr from pod list")
}

func parseCIDRFromString(content string) (result []*net.IPNet) {
	ipv4 := regexp.MustCompile(v4).FindAllString(content, -1)
	ipv6 := regexp.MustCompile(v6).FindAllString(content, -1)

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
	return result
}
