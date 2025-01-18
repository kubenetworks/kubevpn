package inject

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/sets"
	pkgresource "k8s.io/cli-runtime/pkg/resource"
	runtimeresource "k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// InjectEnvoySidecar patch a sidecar, using iptables to do port-forward let this pod decide should go to 233.254.254.100 or request to 127.0.0.1
// https://istio.io/latest/docs/ops/deployment/requirements/#ports-used-by-istio
func InjectEnvoySidecar(ctx context.Context, f cmdutil.Factory, clientset *kubernetes.Clientset, namespace, workload string, object *runtimeresource.Info, headers map[string]string, portMap []string) (err error) {
	u := object.Object.(*unstructured.Unstructured)
	var templateSpec *v1.PodTemplateSpec
	var path []string
	templateSpec, path, err = util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	nodeID := fmt.Sprintf("%s.%s", object.Mapping.Resource.GroupResource().String(), object.Name)

	c := util.PodRouteConfig{LocalTunIPv4: "127.0.0.1", LocalTunIPv6: netip.IPv6Loopback().String()}
	ports, portmap := GetPort(templateSpec, portMap)
	port := controlplane.ConvertContainerPort(ports...)
	var containerPort2EnvoyListenerPort = make(map[int32]int32)
	for i := range len(port) {
		randomPort, _ := util.GetAvailableTCPPortOrDie()
		port[i].EnvoyListenerPort = int32(randomPort)
		containerPort2EnvoyListenerPort[port[i].ContainerPort] = int32(randomPort)
	}
	err = addEnvoyConfig(clientset.CoreV1().ConfigMaps(namespace), nodeID, c, headers, port, portmap)
	if err != nil {
		log.Errorf("Failed to add envoy config: %v", err)
		return err
	}

	// already inject container envoy-proxy, do nothing
	containerNames := sets.New[string]()
	for _, container := range templateSpec.Spec.Containers {
		containerNames.Insert(container.Name)
	}
	if containerNames.HasAll(config.ContainerSidecarVPN, config.ContainerSidecarEnvoyProxy) {
		log.Infof("Workload %s/%s has already been injected with sidecar", namespace, workload)
		return
	}

	enableIPv6, _ := util.DetectPodSupportIPv6(ctx, f, namespace)
	// (1) add mesh container
	AddEnvoyContainer(templateSpec, nodeID, enableIPv6)
	helper := pkgresource.NewHelper(object.Client, object.Mapping)
	ps := []P{
		{
			Op:    "replace",
			Path:  "/" + strings.Join(append(path, "spec"), "/"),
			Value: templateSpec.Spec,
		},
	}
	var bytes []byte
	bytes, err = k8sjson.Marshal(append(ps))
	if err != nil {
		return err
	}
	_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{})
	if err != nil {
		log.Errorf("Failed to patch resource: %s %s, err: %v", object.Mapping.Resource.Resource, object.Name, err)
		return err
	}
	log.Infof("Patching workload %s", workload)
	err = util.RolloutStatus(ctx, f, namespace, workload, time.Minute*60)
	if err != nil {
		return err
	}

	// 2) modify service containerPort to envoy listener port
	err = ModifyServiceTargetPort(ctx, clientset, namespace, labels.SelectorFromSet(templateSpec.Labels).String(), containerPort2EnvoyListenerPort)
	if err != nil {
		return err
	}
	return nil
}

func ModifyServiceTargetPort(ctx context.Context, clientset *kubernetes.Clientset, namespace string, labels string, m map[int32]int32) error {
	list, err := clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{LabelSelector: labels})
	if err != nil {
		return err
	}
	if len(list.Items) == 0 {
		return fmt.Errorf("can not found service with label: %v", labels)
	}
	for i := range len(list.Items[0].Spec.Ports) {
		list.Items[0].Spec.Ports[i].TargetPort = intstr.FromInt32(m[list.Items[0].Spec.Ports[i].Port])
	}
	_, err = clientset.CoreV1().Services(namespace).Update(ctx, &list.Items[0], metav1.UpdateOptions{})
	return err
}

func GetPort(templateSpec *v1.PodTemplateSpec, portMaps []string) ([]v1.ContainerPort, map[int32]string) {
	var ports []v1.ContainerPort
	for _, container := range templateSpec.Spec.Containers {
		ports = append(ports, container.Ports...)
	}
	var found = func(containerPort int32) bool {
		for _, port := range ports {
			if port.ContainerPort == containerPort {
				return true
			}
		}
		return false
	}
	for _, portMap := range portMaps {
		port := util.ParsePort(portMap)
		port.HostPort = 0
		if port.ContainerPort != 0 && !found(port.ContainerPort) {
			ports = append(ports, port)
		}
	}

	var portmap = make(map[int32]string)
	for _, port := range ports {
		randomPort, _ := util.GetAvailableTCPPortOrDie()
		portmap[port.ContainerPort] = fmt.Sprintf("%d:%d", randomPort, port.ContainerPort)
	}
	for _, portMap := range portMaps {
		port := util.ParsePort(portMap)
		if port.ContainerPort != 0 {
			randomPort, _ := util.GetAvailableTCPPortOrDie()
			portmap[port.ContainerPort] = fmt.Sprintf("%d:%d", randomPort, port.HostPort)
		}
	}
	return ports, portmap
}

var _ = `function EPHEMERAL_PORT() {
    UPORT=65535
    LPORT=30000
    while true; do
        CANDIDATE=$[$LPORT + ($RANDOM % ($UPORT-$LPORT))]
        (echo -n >/dev/tcp/127.0.0.1/${CANDIDATE}) >/dev/null 2>&1
        if [ $? -ne 0 ]; then
            echo $CANDIDATE
            break
        fi
    done
}`

func NewMapper(clientset *kubernetes.Clientset, ns string, labels string, headers map[string]string, workloads []string) *Mapper {
	ctx, cancelFunc := context.WithCancel(context.Background())
	return &Mapper{
		ns:        ns,
		headers:   headers,
		workloads: workloads,
		labels:    labels,
		ctx:       ctx,
		cancel:    cancelFunc,
		clientset: clientset,
	}
}

type Mapper struct {
	ns        string
	headers   map[string]string
	workloads []string
	labels    string

	ctx    context.Context
	cancel context.CancelFunc

	clientset *kubernetes.Clientset
}

func (m *Mapper) Run() {
	var podNameCtx = &sync.Map{}
	defer func() {
		podNameCtx.Range(func(key, value any) bool {
			value.(context.CancelFunc)()
			return true
		})
		podNameCtx.Clear()
	}()

	var lastLocalPort2EnvoyRulePort map[int32]int32
	for m.ctx.Err() == nil {
		localPort2EnvoyRulePort, err := m.getLocalPort2EnvoyRulePort()
		if err != nil {
			log.Errorf("failed to get local port to envoy rule port: %v", err)
			time.Sleep(time.Second * 2)
			continue
		}
		if !reflect.DeepEqual(localPort2EnvoyRulePort, lastLocalPort2EnvoyRulePort) {
			podNameCtx.Range(func(key, value any) bool {
				value.(context.CancelFunc)()
				return true
			})
			podNameCtx.Clear()
		}
		lastLocalPort2EnvoyRulePort = localPort2EnvoyRulePort

		list, err := util.GetRunningPodList(m.ctx, m.clientset, m.ns, m.labels)
		if err != nil {
			log.Errorf("failed to list running pod: %v", err)
			time.Sleep(time.Second * 2)
			continue
		}
		podNames := sets.New[string]()
		for _, pod := range list {
			podNames.Insert(pod.Name)
			if _, ok := podNameCtx.Load(pod.Name); ok {
				continue
			}

			containerNames := sets.New[string]()
			for _, container := range pod.Spec.Containers {
				containerNames.Insert(container.Name)
			}
			if !containerNames.HasAny(config.ContainerSidecarVPN, config.ContainerSidecarEnvoyProxy) {
				log.Infof("Labels with pod have been reset")
				return
			}

			podIP, err := netip.ParseAddr(pod.Status.PodIP)
			if err != nil {
				continue
			}

			ctx, cancel := context.WithCancel(m.ctx)
			podNameCtx.Store(pod.Name, cancel)

			go func(remoteSSHServer netip.AddrPort, podName string) {
				for containerPort, envoyRulePort := range localPort2EnvoyRulePort {
					go func(containerPort, envoyRulePort int32) {
						local := netip.AddrPortFrom(netip.IPv4Unspecified(), uint16(containerPort))
						remote := netip.AddrPortFrom(netip.IPv4Unspecified(), uint16(envoyRulePort))
						for ctx.Err() == nil {
							_ = ssh.ExposeLocalPortToRemote(ctx, remoteSSHServer, remote, local)
							time.Sleep(time.Second * 1)
						}
					}(containerPort, envoyRulePort)
				}
			}(netip.AddrPortFrom(podIP, 2222), pod.Name)
		}
		podNameCtx.Range(func(key, value any) bool {
			if !podNames.Has(key.(string)) {
				value.(context.CancelFunc)()
				podNameCtx.Delete(key.(string))
			}
			return true
		})
		time.Sleep(time.Second * 2)
	}
}

func (m *Mapper) getLocalPort2EnvoyRulePort() (map[int32]int32, error) {
	configMap, err := m.clientset.CoreV1().ConfigMaps(m.ns).Get(m.ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var v = make([]*controlplane.Virtual, 0)
	if str, ok := configMap.Data[config.KeyEnvoy]; ok {
		if err = yaml.Unmarshal([]byte(str), &v); err != nil {
			return nil, err
		}
	}

	uidList := sets.New[string]()
	for _, workload := range m.workloads {
		uidList.Insert(util.ConvertWorkloadToUid(workload))
	}
	var localPort2EnvoyRulePort = make(map[int32]int32)
	for _, virtual := range v {
		if uidList.Has(virtual.Uid) {
			for _, rule := range virtual.Rules {
				if reflect.DeepEqual(m.headers, rule.Headers) {
					for containerPort, portPair := range rule.PortMap {
						if strings.Index(portPair, ":") > 0 {
							split := strings.Split(portPair, ":")
							if len(split) == 2 {
								envoyRulePort, _ := strconv.Atoi(split[0])
								localPort, _ := strconv.Atoi(split[1])
								localPort2EnvoyRulePort[int32(localPort)] = int32(envoyRulePort)
							}
						} else {
							envoyRulePort, _ := strconv.Atoi(portPair)
							localPort2EnvoyRulePort[containerPort] = int32(envoyRulePort)
						}
					}
				}
			}
		}
	}
	return localPort2EnvoyRulePort, nil
}

func (m *Mapper) Stop(workload string) {
	if m == nil {
		return
	}
	if !sets.New[string]().Insert(m.workloads...).Has(workload) {
		return
	}
	m.cancel()
}

func (m *Mapper) IsMe(uid string, headers map[string]string) bool {
	if m == nil {
		return false
	}
	if !sets.New[string]().Insert(m.workloads...).Has(util.ConvertUidToWorkload(uid)) {
		return false
	}
	if !reflect.DeepEqual(m.headers, headers) {
		return false
	}
	return true
}
