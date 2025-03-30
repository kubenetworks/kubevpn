package handler

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type Proxy struct {
	headers   map[string]string
	portMap   []string
	workload  string
	namespace string

	portMapper *Mapper
}

type ProxyList []*Proxy

func (l *ProxyList) Remove(ns, workload string) {
	if l == nil {
		return
	}
	for i := 0; i < len(*l); i++ {
		p := (*l)[i]
		if p.workload == workload && p.namespace == ns {
			*l = append((*l)[:i], (*l)[i+1:]...)
			i--
		}
		p.portMapper.Stop()
	}
}

func (l *ProxyList) Add(connectNamespace string, proxy *Proxy) {
	go proxy.portMapper.Run(connectNamespace)
	*l = append(*l, proxy)
}

func (l *ProxyList) IsMe(ns, uid string, headers map[string]string) bool {
	if l == nil {
		return false
	}
	for _, proxy := range *l {
		if proxy.workload == util.ConvertUidToWorkload(uid) &&
			proxy.namespace == ns &&
			reflect.DeepEqual(proxy.headers, headers) {
			return true
		}
	}
	return false
}

func NewMapper(clientset *kubernetes.Clientset, ns string, labels string, headers map[string]string, workload string) *Mapper {
	ctx, cancelFunc := context.WithCancel(context.Background())
	return &Mapper{
		ns:        ns,
		headers:   headers,
		workload:  workload,
		labels:    labels,
		ctx:       ctx,
		cancel:    cancelFunc,
		clientset: clientset,
	}
}

type Mapper struct {
	ns       string
	headers  map[string]string
	workload string
	labels   string

	ctx       context.Context
	cancel    context.CancelFunc
	clientset *kubernetes.Clientset
}

func (m *Mapper) Run(connectNamespace string) {
	if m == nil {
		return
	}
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
		localPort2EnvoyRulePort, err := m.getLocalPort2EnvoyRulePort(connectNamespace)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				continue
			}
			plog.G(m.ctx).Errorf("failed to get local port to envoy rule port: %v", err)
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
			if errors.Is(err, context.Canceled) {
				continue
			}
			plog.G(m.ctx).Errorf("failed to list running pod: %v", err)
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
				plog.G(m.ctx).Infof("Labels with pod have been reset")
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
							time.Sleep(time.Second * 2)
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

func (m *Mapper) getLocalPort2EnvoyRulePort(connectNamespace string) (map[int32]int32, error) {
	configMap, err := m.clientset.CoreV1().ConfigMaps(connectNamespace).Get(m.ctx, config.ConfigMapPodTrafficManager, v1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var v = make([]*controlplane.Virtual, 0)
	if str, ok := configMap.Data[config.KeyEnvoy]; ok {
		if err = yaml.Unmarshal([]byte(str), &v); err != nil {
			return nil, err
		}
	}

	var localPort2EnvoyRulePort = make(map[int32]int32)
	for _, virtual := range v {
		if util.ConvertWorkloadToUid(m.workload) == virtual.Uid && m.ns == virtual.Namespace {
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

func (m *Mapper) Stop() {
	if m == nil || m.cancel == nil {
		return
	}
	m.cancel()
}
