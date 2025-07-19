package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	libconfig "github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/netutil"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/cmd/util/podcmd"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/syncthing"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type CloneOptions struct {
	Namespace      string
	Headers        map[string]string
	Workloads      []string
	ExtraRouteInfo ExtraRouteInfo
	Engine         config.Engine

	TargetContainer     string
	TargetImage         string
	TargetWorkloadNames map[string]string

	OriginKubeconfigPath string
	LocalDir             string
	RemoteDir            string
	Request              *rpc.CloneRequest `json:"Request,omitempty"`

	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	config     *rest.Config
	factory    cmdutil.Factory

	rollbackFuncList []func() error
	ctx              context.Context
	syncthingGUIAddr string
}

func (d *CloneOptions) InitClient(f cmdutil.Factory) (err error) {
	d.factory = f
	if d.config, err = d.factory.ToRESTConfig(); err != nil {
		return
	}
	if d.restclient, err = d.factory.RESTClient(); err != nil {
		return
	}
	if d.clientset, err = d.factory.KubernetesClientSet(); err != nil {
		return
	}
	if d.Namespace, _, err = d.factory.ToRawKubeConfigLoader().Namespace(); err != nil {
		return
	}
	return
}

func (d *CloneOptions) SetContext(ctx context.Context) {
	d.ctx = ctx
}

// DoClone
/*
* 1) download mount path use empty-dir but skip empty-dir in init-containers
* 2) get env from containers
* 3) create serviceAccount as needed
* 4) modify podTempSpec inject kubevpn container
 */
func (d *CloneOptions) DoClone(ctx context.Context, kubeconfigJsonBytes []byte, image string) error {
	var args []string
	if len(d.Headers) != 0 {
		args = append(args, "--headers", labels.Set(d.Headers).String())
	}
	for _, workload := range d.Workloads {
		plog.G(ctx).Infof("Clone workload %s", workload)
		_, controller, err := util.GetTopOwnerObject(ctx, d.factory, d.Namespace, workload)
		if err != nil {
			return err
		}
		u := controller.Object.(*unstructured.Unstructured)
		if err = unstructured.SetNestedField(u.UnstructuredContent(), int64(1), "spec", "replicas"); err != nil {
			plog.G(ctx).Warnf("Failed to set repilcaset to 1: %v", err)
		}
		RemoveUselessInfo(u)
		var newUUID uuid.UUID
		newUUID, err = uuid.NewUUID()
		if err != nil {
			return err
		}
		originName := u.GetName()
		u.SetName(fmt.Sprintf("%s-clone-%s", u.GetName(), newUUID.String()[:5]))
		d.TargetWorkloadNames[workload] = fmt.Sprintf("%s/%s", controller.Mapping.Resource.GroupResource().Resource, u.GetName())
		labelsMap := map[string]string{
			config.ManageBy:   config.ConfigMapPodTrafficManager,
			"owner-ref":       u.GetName(),
			"origin-workload": originName,
		}
		u.SetLabels(labels.Merge(u.GetLabels(), labelsMap))
		var path []string
		var spec *v1.PodTemplateSpec
		spec, path, err = util.GetPodTemplateSpecPath(u)
		if err != nil {
			return err
		}

		err = unstructured.SetNestedStringMap(u.Object, labelsMap, "spec", "selector", "matchLabels")
		if err != nil {
			return err
		}
		var client dynamic.Interface
		client, err = d.factory.DynamicClient()
		if err != nil {
			return err
		}
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// (1) add annotation KUBECONFIG
			anno := spec.GetAnnotations()
			if anno == nil {
				anno = map[string]string{}
			}
			anno[config.KUBECONFIG] = string(kubeconfigJsonBytes)
			spec.SetAnnotations(anno)

			// (2) modify labels
			spec.SetLabels(labelsMap)

			// (3) add volumes KUBECONFIG and syncDir
			syncDataDirName := config.VolumeSyncthing
			spec.Spec.Volumes = append(spec.Spec.Volumes,
				v1.Volume{
					Name: config.KUBECONFIG,
					VolumeSource: v1.VolumeSource{
						DownwardAPI: &v1.DownwardAPIVolumeSource{
							Items: []v1.DownwardAPIVolumeFile{{
								Path: config.KUBECONFIG,
								FieldRef: &v1.ObjectFieldSelector{
									FieldPath: fmt.Sprintf("metadata.annotations['%s']", config.KUBECONFIG),
								},
							}},
						},
					},
				},
				v1.Volume{
					Name: syncDataDirName,
					VolumeSource: v1.VolumeSource{
						EmptyDir: &v1.EmptyDirVolumeSource{},
					},
				},
			)

			// (4) add kubevpn containers
			containers := spec.Spec.Containers
			// remove vpn sidecar
			for i := 0; i < len(containers); i++ {
				containerName := containers[i].Name
				if err == nil && (containerName == config.ContainerSidecarVPN || containerName == config.ContainerSidecarEnvoyProxy) {
					containers = append(containers[:i], containers[i+1:]...)
					i--
				}
			}
			{
				container, err := podcmd.FindOrDefaultContainerByName(&v1.Pod{Spec: v1.PodSpec{Containers: containers}}, d.TargetContainer, false, plog.G(ctx).Out)
				if err != nil {
					return err
				}
				if d.TargetImage != "" {
					// update container[index] image
					container.Image = d.TargetImage
				}
				if d.LocalDir != "" {
					container.Command = []string{"tail", "-f", "/dev/null"}
					container.Args = []string{}
					container.VolumeMounts = append(container.VolumeMounts, v1.VolumeMount{
						Name:      syncDataDirName,
						ReadOnly:  false,
						MountPath: d.RemoteDir,
					})
					container.LivenessProbe = nil
					container.ReadinessProbe = nil
					container.StartupProbe = nil
				}
			}
			// https://kubernetes.io/docs/tasks/administer-cluster/sysctl-cluster/
			if spec.Spec.SecurityContext == nil {
				spec.Spec.SecurityContext = &v1.PodSecurityContext{}
			}
			// https://kubernetes.io/docs/tasks/administer-cluster/sysctl-cluster/#enabling-unsafe-sysctls
			// kubelet --allowed-unsafe-sysctls \
			//  'kernel.msg*,net.core.somaxconn' ...
			/*spec.Spec.SecurityContext.Sysctls = append(spec.Spec.SecurityContext.Sysctls, []v1.Sysctl{
				{
					Name:  "net.ipv4.ip_forward",
					Value: "1",
				},
				{
					Name:  "net.ipv6.conf.all.disable_ipv6",
					Value: "0",
				},
				{
					Name:  "net.ipv6.conf.all.forwarding",
					Value: "1",
				},
				{
					Name:  "net.ipv4.conf.all.route_localnet",
					Value: "1",
				},
			}...)*/
			container := genVPNContainer(workload, d.Engine, d.Namespace, image, args)
			containerSync := genSyncthingContainer(d.RemoteDir, syncDataDirName, image)
			spec.Spec.Containers = append(containers, *container, *containerSync)
			//set spec
			marshal, err := json.Marshal(spec)
			if err != nil {
				return err
			}
			m := make(map[string]interface{})
			err = json.Unmarshal(marshal, &m)
			if err != nil {
				return err
			}
			if err = unstructured.SetNestedField(u.Object, m, path...); err != nil {
				return err
			}

			_, createErr := client.Resource(controller.Mapping.Resource).Namespace(d.Namespace).Create(context.Background(), u, metav1.CreateOptions{})
			return createErr
		})
		if retryErr != nil {
			return fmt.Errorf("create clone for resource %s failed: %v", workload, retryErr)
		}
		plog.G(ctx).Infof("Create clone resource %s/%s in target cluster", u.GetObjectKind().GroupVersionKind().GroupKind().String(), u.GetName())
		plog.G(ctx).Infof("Wait for clone resource %s/%s to be ready", u.GetObjectKind().GroupVersionKind().GroupKind().String(), u.GetName())
		plog.G(ctx).Infoln()
		err = util.WaitPodToBeReady(ctx, d.clientset.CoreV1().Pods(d.Namespace), metav1.LabelSelector{MatchLabels: labelsMap})
		if err != nil {
			return err
		}
		_ = util.RolloutStatus(ctx, d.factory, d.Namespace, workload, time.Minute*60)

		if d.LocalDir != "" {
			err = d.SyncDir(ctx, fields.SelectorFromSet(labelsMap).String())
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func genSyncthingContainer(remoteDir string, syncDataDirName string, image string) *v1.Container {
	containerSync := &v1.Container{
		Name:  config.ContainerSidecarSyncthing,
		Image: image,
		// https://stackoverflow.com/questions/32918849/what-process-signal-does-pod-receive-when-executing-kubectl-rolling-update
		Command: []string{
			"kubevpn",
			"syncthing",
			"--dir",
			remoteDir,
		},
		Resources: v1.ResourceRequirements{
			Requests: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("500m"),
				v1.ResourceMemory: resource.MustParse("512Mi"),
			},
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("1000m"),
				v1.ResourceMemory: resource.MustParse("1024Mi"),
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      syncDataDirName,
				ReadOnly:  false,
				MountPath: remoteDir,
			},
		},
		// only for:
		// panic: mkdir /.kubevpn: permission denied                                                                                                                                │
		//                                                                                                                                                                          │
		// goroutine 1 [running]:                                                                                                                                                   │
		// github.com/wencaiwulue/kubevpn/v2/pkg/config.init.1()                                                                                                                    │
		//     /go/src/github.com/wencaiwulue/kubevpn/pkg/config/const.go:34 +0x1ae
		SecurityContext: &v1.SecurityContext{
			RunAsUser:  ptr.To[int64](0),
			RunAsGroup: ptr.To[int64](0),
		},
	}
	return containerSync
}

func genVPNContainer(workload string, engine config.Engine, namespace string, image string, args []string) *v1.Container {
	container := &v1.Container{
		Name:  config.ContainerSidecarVPN,
		Image: image,
		// https://stackoverflow.com/questions/32918849/what-process-signal-does-pod-receive-when-executing-kubectl-rolling-update
		Command: append([]string{
			"kubevpn",
			"proxy",
			workload,
			"--kubeconfig", "/tmp/.kube/" + config.KUBECONFIG,
			"--namespace", namespace,
			"--image", image,
			"--foreground",
		}, args...),
		Env: []v1.EnvVar{},
		Resources: v1.ResourceRequirements{
			Requests: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("1000m"),
				v1.ResourceMemory: resource.MustParse("1024Mi"),
			},
			Limits: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("2000m"),
				v1.ResourceMemory: resource.MustParse("2048Mi"),
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      config.KUBECONFIG,
				ReadOnly:  false,
				MountPath: "/tmp/.kube",
			},
		},
		Lifecycle: &v1.Lifecycle{
			PostStart: &v1.LifecycleHandler{
				Exec: &v1.ExecAction{
					Command: []string{
						"/bin/bash",
						"-c",
						`
echo 1 > /proc/sys/net/ipv4/ip_forward
echo 0 > /proc/sys/net/ipv6/conf/all/disable_ipv6
echo 1 > /proc/sys/net/ipv6/conf/all/forwarding
echo 1 > /proc/sys/net/ipv4/conf/all/route_localnet
update-alternatives --set iptables /usr/sbin/iptables-legacy`,
					},
				},
			},
		},
		ImagePullPolicy: v1.PullIfNotPresent,
		SecurityContext: &v1.SecurityContext{
			Capabilities: &v1.Capabilities{
				Add: []v1.Capability{
					"NET_ADMIN",
				},
			},
			RunAsUser:  pointer.Int64(0),
			RunAsGroup: pointer.Int64(0),
			Privileged: pointer.Bool(true),
		},
	}
	return container
}

func (d *CloneOptions) SyncDir(ctx context.Context, labels string) error {
	list, err := util.GetRunningPodList(ctx, d.clientset, d.Namespace, labels)
	if err != nil {
		return err
	}
	remoteAddr := net.JoinHostPort(list[0].Status.PodIP, strconv.Itoa(libconfig.DefaultTCPPort))
	localPort, _ := util.GetAvailableTCPPortOrDie()
	localAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
	err = syncthing.StartClient(d.ctx, d.LocalDir, localAddr, remoteAddr)
	if err != nil {
		return err
	}
	d.syncthingGUIAddr = (&url.URL{Scheme: "http", Host: localAddr}).String()
	plog.G(ctx).Infof("Access the syncthing GUI via the following URL: %s", d.syncthingGUIAddr)
	go func() {
		client := syncthing.NewClient(localAddr)
		podName := list[0].Name
		for d.ctx.Err() == nil {
			func() {
				defer time.Sleep(time.Second * 2)

				sortBy := func(pods []*v1.Pod) sort.Interface { return sort.Reverse(podutils.ActivePods(pods)) }
				_, _, _ = polymorphichelpers.GetFirstPod(d.clientset.CoreV1(), d.Namespace, labels, time.Second*30, sortBy)
				list, err := util.GetRunningPodList(d.ctx, d.clientset, d.Namespace, labels)
				if err != nil {
					plog.G(ctx).Error(err)
					return
				}
				if podName == list[0].Name {
					return
				}

				podName = list[0].Name
				plog.G(ctx).Debugf("Detect newer pod %s", podName)
				var conf *libconfig.Configuration
				conf, err = client.GetConfig(d.ctx)
				if err != nil {
					plog.G(ctx).Errorf("Failed to get config from syncthing: %v", err)
					return
				}
				for i := range conf.Devices {
					if config.RemoteDeviceID.Equals(conf.Devices[i].DeviceID) {
						addr := netutil.AddressURL("tcp", net.JoinHostPort(list[0].Status.PodIP, strconv.Itoa(libconfig.DefaultTCPPort)))
						conf.Devices[i].Addresses = []string{addr}
						plog.G(ctx).Debugf("Use newer remote syncthing endpoint: %s", addr)
					}
				}
				err = client.PutConfig(d.ctx, conf)
				if err != nil {
					plog.G(ctx).Errorf("Failed to set config to syncthing: %v", err)
				}
			}()
		}
	}()
	return nil
}

func RemoveUselessInfo(u *unstructured.Unstructured) {
	if u == nil {
		return
	}

	delete(u.Object, "status")
	_ = unstructured.SetNestedField(u.Object, nil, "status")

	u.SetManagedFields(nil)
	u.SetResourceVersion("")
	u.SetCreationTimestamp(metav1.NewTime(time.Time{}))
	u.SetUID("")
	u.SetGeneration(0)
	a := u.GetAnnotations()
	if len(a) == 0 {
		a = map[string]string{}
	}
	delete(a, "kubectl.kubernetes.io/last-applied-configuration")
	u.SetAnnotations(a)
}

func (d *CloneOptions) Cleanup(ctx context.Context, workloads ...string) error {
	if len(workloads) == 0 {
		for _, v := range d.TargetWorkloadNames {
			workloads = append(workloads, v)
		}
	}
	for _, workload := range workloads {
		plog.G(ctx).Infof("Cleaning up clone workload: %s", workload)
		object, err := util.GetUnstructuredObject(d.factory, d.Namespace, workload)
		if err != nil {
			plog.G(ctx).Errorf("Failed to get unstructured object: %s", err)
			return err
		}
		var client dynamic.Interface
		client, err = d.factory.DynamicClient()
		if err != nil {
			plog.G(ctx).Errorf("Failed to get dynamic client: %v", err)
			return err
		}
		err = client.Resource(object.Mapping.Resource).Namespace(d.Namespace).Delete(context.Background(), object.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			plog.G(ctx).Errorf("Failed to delete clone object: %v", err)
			return err
		}
		plog.G(ctx).Infof("Deleted clone object: %s/%s", object.Mapping.Resource.GroupResource().Resource, object.Name)
	}
	for _, f := range d.rollbackFuncList {
		if f != nil {
			if err := f(); err != nil {
				plog.G(ctx).Warnf("Failed to exec rollback function: %s", err)
			}
		}
	}
	return nil
}

func (d *CloneOptions) AddRollbackFunc(f func() error) {
	d.rollbackFuncList = append(d.rollbackFuncList, f)
}

func (d *CloneOptions) GetFactory() cmdutil.Factory {
	return d.factory
}

func (d *CloneOptions) GetSyncthingGUIAddr() string {
	return d.syncthingGUIAddr
}
