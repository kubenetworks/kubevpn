package pkg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/pkg/exchange"
	"github.com/wencaiwulue/kubevpn/util"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	pkgresource "k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/podutils"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

func CreateOutboundPod(clientset *kubernetes.Clientset, namespace string, trafficManagerIP string, nodeCIDR []*net.IPNet) (net.IP, error) {
	manager, _, err := polymorphichelpers.GetFirstPod(clientset.CoreV1(),
		namespace,
		fields.OneTermEqualSelector("app", util.TrafficManager).String(),
		time.Second*5,
		func(pods []*v1.Pod) sort.Interface {
			return sort.Reverse(podutils.ActivePods(pods))
		},
	)

	if err == nil && manager != nil {
		log.Infoln("traffic manager already exist, reuse it")
		UpdateRefCount(clientset, namespace, manager.Name, 1)
		return net.ParseIP(manager.Status.PodIP), nil
	}
	log.Infoln("traffic manager not exist, try to create it...")
	args := []string{
		"sysctl net.ipv4.ip_forward=1",
		"iptables -F",
		"iptables -P INPUT ACCEPT",
		"iptables -P FORWARD ACCEPT",
		"iptables -t nat -A POSTROUTING -s " + util.CIDR.String() + " -o eth0 -j MASQUERADE",
	}
	for _, ipNet := range nodeCIDR {
		args = append(args, "iptables -t nat -A POSTROUTING -s "+ipNet.String()+" -o eth0 -j MASQUERADE")
	}
	args = append(args, "kubevpn serve -L tcp://:10800 -L tun://:8421?net="+trafficManagerIP+" --debug=true")

	t := true
	zero := int64(0)
	manager = &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        util.TrafficManager,
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
	newPod, err := clientset.CoreV1().Pods(namespace).Create(context.TODO(), manager, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	if newPod.Status.Phase == v1.PodRunning {
		return net.ParseIP(newPod.Status.PodIP), nil
	}
	watchStream, err := clientset.CoreV1().Pods(namespace).Watch(context.TODO(), metav1.SingleObject(metav1.ObjectMeta{Name: manager.GetName()}))
	if err != nil {
		return nil, err
	}
	defer watchStream.Stop()
	var phase v1.PodPhase
	for {
		select {
		case e := <-watchStream.ResultChan():
			if podT, ok := e.Object.(*v1.Pod); ok {
				if phase != podT.Status.Phase {
					log.Infof("pod %s status is %s", util.TrafficManager, podT.Status.Phase)
				}
				if podT.Status.Phase == v1.PodRunning {
					return net.ParseIP(podT.Status.PodIP), nil
				}
				phase = podT.Status.Phase
			}
		case <-time.Tick(time.Minute * 10):
			return nil, errors.New(fmt.Sprintf("wait pod %s to be ready timeout", util.TrafficManager))
		}
	}
}

func CreateInboundPod(factory cmdutil.Factory, namespace, workloads string, config util.PodRouteConfig) error {
	object, err := util.GetUnstructuredObject(factory, namespace, workloads)
	if err != nil {
		return err
	}

	u := object.Object.(*unstructured.Unstructured)

	podTempSpec, path, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	origin := *podTempSpec

	helper := pkgresource.NewHelper(object.Client, object.Mapping)

	exchange.AddContainer(&podTempSpec.Spec, config)

	// pods without controller
	if len(path) == 0 {
		podTempSpec.Spec.PriorityClassName = ""
		for _, c := range podTempSpec.Spec.Containers {
			c.LivenessProbe = nil
			c.StartupProbe = nil
			c.ReadinessProbe = nil
		}
		p := &v1.Pod{ObjectMeta: podTempSpec.ObjectMeta, Spec: podTempSpec.Spec}
		CleanupUselessInfo(p)
		if err = createAfterDeletePod(factory, p, helper); err != nil {
			return err
		}

		rollbackFuncList = append(rollbackFuncList, func() {
			p2 := &v1.Pod{ObjectMeta: origin.ObjectMeta, Spec: origin.Spec}
			CleanupUselessInfo(p2)
			if err = createAfterDeletePod(factory, p2, helper); err != nil {
				log.Error(err)
			}
		})
	} else
	// controllers
	{
		bytes, _ := json.Marshal([]struct {
			Op    string      `json:"op"`
			Path  string      `json:"path"`
			Value interface{} `json:"value"`
		}{{
			Op:    "replace",
			Path:  "/" + strings.Join(append(path, "spec"), "/"),
			Value: podTempSpec.Spec,
		}})
		_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{})
		if err != nil {
			log.Errorf("error while inject proxy container, err: %v, exiting...")
			return err
		}
		removePatch, restorePatch := patch(origin, path)
		_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, removePatch, &metav1.PatchOptions{})
		if err != nil {
			log.Warnf("error while remove probe of resource: %s %s, ignore, err: %v",
				object.Mapping.GroupVersionKind.GroupKind().String(), object.Name, err)
		}
		rollbackFuncList = append(rollbackFuncList, func() {
			if err = removeInboundContainer(factory, namespace, workloads); err != nil {
				log.Error(err)
			}
			if _, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, restorePatch, &metav1.PatchOptions{}); err != nil {
				log.Warnf("error while restore probe of resource: %s %s, ignore, err: %v",
					object.Mapping.GroupVersionKind.GroupKind().String(), object.Name, err)
			}
		})
	}
	_ = util.RolloutStatus(factory, namespace, workloads, time.Minute*5)
	return err
}

func createAfterDeletePod(factory cmdutil.Factory, p *v1.Pod, helper *pkgresource.Helper) error {
	zero := int64(0)
	if _, err := helper.DeleteWithOptions(p.Namespace, p.Name, &metav1.DeleteOptions{
		GracePeriodSeconds: &zero,
	}); err != nil {
		log.Error(err)
	}
	if err := retry.OnError(wait.Backoff{
		Steps:    10,
		Duration: 50 * time.Millisecond,
		Factor:   5.0,
		Jitter:   1,
	}, func(err error) bool {
		if !k8serrors.IsAlreadyExists(err) {
			return true
		}
		clientset, err := factory.KubernetesClientSet()
		get, err := clientset.CoreV1().Pods(p.Namespace).Get(context.TODO(), p.Name, metav1.GetOptions{})
		if err != nil || get.Status.Phase != v1.PodRunning {
			return true
		}
		return false
	}, func() error {
		if _, err := helper.Create(p.Namespace, true, p); err != nil {
			return err
		}
		return errors.New("")
	}); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	return nil
}

func removeInboundContainer(factory cmdutil.Factory, namespace, workloads string) error {
	object, err := util.GetUnstructuredObject(factory, namespace, workloads)
	if err != nil {
		return err
	}

	u := object.Object.(*unstructured.Unstructured)

	podTempSpec, path, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	helper := pkgresource.NewHelper(object.Client, object.Mapping)

	// pods
	zero := int64(0)
	if len(path) == 0 {
		_, err = helper.DeleteWithOptions(object.Namespace, object.Name, &metav1.DeleteOptions{
			GracePeriodSeconds: &zero,
		})
		if err != nil {
			return err
		}
	}
	// how to scale to one
	exchange.RemoveContainer(&podTempSpec.Spec)

	bytes, err := json.Marshal([]struct {
		Op    string      `json:"op"`
		Path  string      `json:"path"`
		Value interface{} `json:"value"`
	}{{
		Op:    "replace",
		Path:  "/" + strings.Join(append(path, "spec"), "/"),
		Value: podTempSpec.Spec,
	}})
	if err != nil {
		return err
	}
	//t := true
	_, err = helper.Patch(object.Namespace, object.Name, types.JSONPatchType, bytes, &metav1.PatchOptions{
		//Force: &t,
	})
	return err
}

func CleanupUselessInfo(pod *v1.Pod) {
	pod.SetSelfLink("")
	pod.SetGeneration(0)
	pod.SetResourceVersion("")
	pod.SetUID("")
	pod.SetDeletionTimestamp(nil)
	pod.SetSelfLink("")
	pod.SetManagedFields(nil)
	pod.SetOwnerReferences(nil)
}

func patch(spec v1.PodTemplateSpec, path []string) (removePatch []byte, restorePatch []byte) {
	type P struct {
		Op    string      `json:"op,omitempty"`
		Path  string      `json:"path,omitempty"`
		Value interface{} `json:"value,omitempty"`
	}
	var remove, restore []P
	for i := range spec.Spec.Containers {
		index := strconv.Itoa(i)
		readinessPath := "/" + strings.Join(append(path, "spec", "containers", index, "readinessProbe"), "/")
		livenessPath := "/" + strings.Join(append(path, "spec", "containers", index, "livenessProbe"), "/")
		startupPath := "/" + strings.Join(append(path, "spec", "containers", index, "startupProbe"), "/")
		remove = append(remove, P{
			Op:    "replace",
			Path:  readinessPath,
			Value: nil,
		}, P{
			Op:    "replace",
			Path:  livenessPath,
			Value: nil,
		}, P{
			Op:    "replace",
			Path:  startupPath,
			Value: nil,
		})
		restore = append(restore, P{
			Op:    "replace",
			Path:  readinessPath,
			Value: spec.Spec.Containers[i].ReadinessProbe,
		}, P{
			Op:    "replace",
			Path:  livenessPath,
			Value: spec.Spec.Containers[i].LivenessProbe,
		}, P{
			Op:    "replace",
			Path:  startupPath,
			Value: spec.Spec.Containers[i].StartupProbe,
		})
	}
	removePatch, _ = json.Marshal(remove)
	restorePatch, _ = json.Marshal(restore)
	return
}
