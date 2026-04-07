package localproxy

import (
	"context"
	"fmt"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type ClusterConnector struct {
	Client           ClusterAPI
	Forwarder        PodDialer
	RESTConfig       *rest.Config
	DefaultNamespace string
}

type resolvedTarget struct {
	PodName   string
	Namespace string
	PodPort   int32
}

type ClusterAPI interface {
	GetService(ctx context.Context, namespace, name string) (*corev1.Service, error)
	GetEndpoints(ctx context.Context, namespace, name string) (*corev1.Endpoints, error)
	ListServices(ctx context.Context) (*corev1.ServiceList, error)
	ListPodsByIP(ctx context.Context, ip string) (*corev1.PodList, error)
}

type PodDialer interface {
	DialPod(ctx context.Context, namespace, podName string, port int32) (net.Conn, error)
}

type kubeClusterAPI struct {
	clientset kubernetes.Interface
}

func NewClusterAPI(config *rest.Config) (ClusterAPI, *kubernetes.Clientset, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}
	return &kubeClusterAPI{clientset: clientset}, clientset, nil
}

func (k *kubeClusterAPI) GetService(ctx context.Context, namespace, name string) (*corev1.Service, error) {
	return k.clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (k *kubeClusterAPI) GetEndpoints(ctx context.Context, namespace, name string) (*corev1.Endpoints, error) {
	return k.clientset.CoreV1().Endpoints(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (k *kubeClusterAPI) ListServices(ctx context.Context) (*corev1.ServiceList, error) {
	return k.clientset.CoreV1().Services("").List(ctx, metav1.ListOptions{})
}

func (k *kubeClusterAPI) ListPodsByIP(ctx context.Context, ip string) (*corev1.PodList, error) {
	return k.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("status.podIP", ip).String(),
	})
}

func (c *ClusterConnector) Connect(ctx context.Context, host string, port int) (net.Conn, error) {
	target, err := c.resolveTarget(ctx, host, port)
	if err != nil {
		return nil, err
	}
	if c.Forwarder == nil {
		return nil, fmt.Errorf("pod dialer is required")
	}
	return c.Forwarder.DialPod(ctx, target.Namespace, target.PodName, target.PodPort)
}

func (c *ClusterConnector) resolveTarget(ctx context.Context, host string, port int) (*resolvedTarget, error) {
	if ip := net.ParseIP(host); ip != nil {
		if target, err := c.resolveServiceIP(ctx, host, port); err == nil {
			return target, nil
		}
		return c.resolvePodIP(ctx, host, port)
	}
	return c.resolveServiceHost(ctx, host, port)
}

func (c *ClusterConnector) resolveServiceHost(ctx context.Context, host string, port int) (*resolvedTarget, error) {
	svcName, ns, ok := parseServiceHost(host, c.DefaultNamespace)
	if !ok {
		return nil, fmt.Errorf("unsupported cluster host %q", host)
	}
	svc, err := c.Client.GetService(ctx, ns, svcName)
	if err != nil {
		return nil, err
	}
	return c.resolveServiceToPod(ctx, svc, port)
}

func (c *ClusterConnector) resolveServiceIP(ctx context.Context, host string, port int) (*resolvedTarget, error) {
	services, err := c.Client.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	for i := range services.Items {
		if services.Items[i].Spec.ClusterIP == host {
			return c.resolveServiceToPod(ctx, &services.Items[i], port)
		}
	}
	return nil, fmt.Errorf("no service found for cluster ip %s", host)
}

func (c *ClusterConnector) resolveServiceToPod(ctx context.Context, svc *corev1.Service, port int) (*resolvedTarget, error) {
	if svc == nil {
		return nil, fmt.Errorf("service is required")
	}
	var servicePort *corev1.ServicePort
	for i := range svc.Spec.Ports {
		if int(svc.Spec.Ports[i].Port) == port {
			servicePort = &svc.Spec.Ports[i]
			break
		}
	}
	if servicePort == nil {
		if len(svc.Spec.Ports) == 1 {
			servicePort = &svc.Spec.Ports[0]
		} else {
			return nil, fmt.Errorf("service %s/%s has no port %d", svc.Namespace, svc.Name, port)
		}
	}

	endpoints, err := c.Client.GetEndpoints(ctx, svc.Namespace, svc.Name)
	if err != nil {
		return nil, err
	}
	var endpointPort *corev1.EndpointPort
	var podName, podNS, podIP string
	for _, subset := range endpoints.Subsets {
		for i := range subset.Ports {
			portRef := &subset.Ports[i]
			if endpointPortMatches(servicePort, portRef, port) {
				endpointPort = portRef
				break
			}
		}
		if endpointPort == nil && len(subset.Ports) == 1 {
			endpointPort = &subset.Ports[0]
		}
		if endpointPort == nil {
			continue
		}
		for _, addr := range subset.Addresses {
			podIP = addr.IP
			if addr.TargetRef != nil && addr.TargetRef.Kind == "Pod" {
				podName = addr.TargetRef.Name
				podNS = addr.TargetRef.Namespace
				break
			}
		}
		if podName != "" || podIP != "" {
			break
		}
	}
	if endpointPort == nil {
		return nil, fmt.Errorf("service %s/%s has no ready endpoints for port %d", svc.Namespace, svc.Name, port)
	}
	if podName == "" {
		if podIP == "" {
			return nil, fmt.Errorf("service %s/%s has endpoints without pod references", svc.Namespace, svc.Name)
		}
		target, err := c.resolvePodIP(ctx, podIP, int(endpointPort.Port))
		if err != nil {
			return nil, err
		}
		target.PodPort = endpointPort.Port
		return target, nil
	}
	if podNS == "" {
		podNS = svc.Namespace
	}
	return &resolvedTarget{
		PodName:   podName,
		Namespace: podNS,
		PodPort:   endpointPort.Port,
	}, nil
}

func endpointPortMatches(servicePort *corev1.ServicePort, endpointPort *corev1.EndpointPort, requestedPort int) bool {
	if servicePort == nil || endpointPort == nil {
		return false
	}
	if servicePort.Name != "" && endpointPort.Name != "" && servicePort.Name == endpointPort.Name {
		return true
	}
	if endpointPort.Port == int32(requestedPort) {
		return true
	}
	if servicePort.TargetPort.IntValue() > 0 && int32(servicePort.TargetPort.IntValue()) == endpointPort.Port {
		return true
	}
	return false
}

func (c *ClusterConnector) resolvePodIP(ctx context.Context, host string, port int) (*resolvedTarget, error) {
	pods, err := c.Client.ListPodsByIP(ctx, host)
	if err != nil {
		return nil, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
			continue
		}
		return &resolvedTarget{
			PodName:   pod.Name,
			Namespace: pod.Namespace,
			PodPort:   int32(port),
		}, nil
	}
	return nil, fmt.Errorf("no running pod found for ip %s", host)
}

func parseServiceHost(host, defaultNamespace string) (service string, namespace string, ok bool) {
	host = strings.TrimSuffix(host, ".")
	parts := strings.Split(host, ".")
	switch {
	case len(parts) >= 5 && parts[2] == "svc" && parts[3] == "cluster" && parts[4] == "local":
		return parts[0], parts[1], true
	case len(parts) >= 3 && parts[2] == "svc":
		return parts[0], parts[1], true
	case len(parts) == 2:
		return parts[0], parts[1], true
	case len(parts) == 1 && defaultNamespace != "":
		return parts[0], defaultNamespace, true
	default:
		return "", "", false
	}
}

func FirstNonLoopbackIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			return ip.String()
		}
	}
	return ""
}
