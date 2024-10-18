package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	libconfig "github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/netutil"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/cmd/util/podcmd"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	"github.com/wencaiwulue/kubevpn/v2/pkg/syncthing"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type CloneOptions struct {
	Namespace      string
	Headers        map[string]string
	Workloads      []string
	ExtraRouteInfo ExtraRouteInfo
	Engine         config.Engine

	TargetKubeconfig       string
	TargetNamespace        string
	TargetContainer        string
	TargetImage            string
	TargetRegistry         string
	IsChangeTargetRegistry bool
	TargetWorkloadNames    map[string]string

	isSame bool

	OriginKubeconfigPath string
	LocalDir             string
	RemoteDir            string

	targetClientset  *kubernetes.Clientset
	targetRestclient *rest.RESTClient
	targetConfig     *rest.Config
	targetFactory    cmdutil.Factory

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

	// init target info
	if len(d.TargetKubeconfig) == 0 {
		d.TargetKubeconfig = d.OriginKubeconfigPath
		d.targetFactory = d.factory
		d.targetClientset = d.clientset
		d.targetConfig = d.config
		d.targetRestclient = d.restclient
		if len(d.TargetNamespace) == 0 {
			d.TargetNamespace = d.Namespace
			d.isSame = true
		}
		return
	}
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.KubeConfig = pointer.String(d.TargetKubeconfig)
	configFlags.Namespace = pointer.String(d.TargetNamespace)
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	d.targetFactory = cmdutil.NewFactory(matchVersionFlags)
	var found bool
	d.TargetNamespace, found, err = d.targetFactory.ToRawKubeConfigLoader().Namespace()
	if err != nil || !found {
		d.TargetNamespace = d.Namespace
	}
	d.targetClientset, err = d.targetFactory.KubernetesClientSet()
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
func (d *CloneOptions) DoClone(ctx context.Context, kubeconfigJsonBytes []byte) error {
	var args []string
	if len(d.Headers) != 0 {
		args = append(args, "--headers", labels.Set(d.Headers).String())
	}
	for _, workload := range d.Workloads {
		log.Infof("Clone workload %s", workload)
		object, err := util.GetUnstructuredObject(d.factory, d.Namespace, workload)
		if err != nil {
			return err
		}
		u := object.Object.(*unstructured.Unstructured)
		if err = unstructured.SetNestedField(u.UnstructuredContent(), int64(1), "spec", "replicas"); err != nil {
			log.Warnf("Failed to set repilcaset to 1: %v", err)
		}
		u.SetNamespace(d.TargetNamespace)
		RemoveUselessInfo(u)
		var newUUID uuid.UUID
		newUUID, err = uuid.NewUUID()
		if err != nil {
			return err
		}
		originName := u.GetName()
		u.SetName(fmt.Sprintf("%s-clone-%s", u.GetName(), newUUID.String()[:5]))
		d.TargetWorkloadNames[workload] = u.GetName()
		// if is another cluster, needs to set volume and set env
		if !d.isSame {
			if err = d.setVolume(u); err != nil {
				return err
			}
			if err = d.setEnv(u); err != nil {
				return err
			}
		}

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
		client, err = d.targetFactory.DynamicClient()
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
				containers[i].ReadinessProbe = nil
				containers[i].LivenessProbe = nil
				containers[i].StartupProbe = nil
				containerName := containers[i].Name
				if err == nil && (containerName == config.ContainerSidecarVPN || containerName == config.ContainerSidecarEnvoyProxy) {
					containers = append(containers[:i], containers[i+1:]...)
					i--
				}
			}
			{
				container, err := podcmd.FindOrDefaultContainerByName(&v1.Pod{Spec: v1.PodSpec{Containers: containers}}, d.TargetContainer, false, log.StandardLogger().Out)
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
			container := &v1.Container{
				Name:  config.ContainerSidecarVPN,
				Image: config.Image,
				// https://stackoverflow.com/questions/32918849/what-process-signal-does-pod-receive-when-executing-kubectl-rolling-update
				Command: append([]string{
					"kubevpn",
					"proxy",
					workload,
					"--kubeconfig", "/tmp/.kube/" + config.KUBECONFIG,
					"--namespace", d.Namespace,
					"--image", config.Image,
					"--engine", string(d.Engine),
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
								"sysctl -w net.ipv4.ip_forward=1\nsysctl -w net.ipv6.conf.all.disable_ipv6=0\nsysctl -w net.ipv6.conf.all.forwarding=1\nsysctl -w net.ipv4.conf.all.route_localnet=1\nupdate-alternatives --set iptables /usr/sbin/iptables-legacy",
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
			containerSync := &v1.Container{
				Name:  config.ContainerSidecarSyncthing,
				Image: config.Image,
				// https://stackoverflow.com/questions/32918849/what-process-signal-does-pod-receive-when-executing-kubectl-rolling-update
				Command: []string{
					"kubevpn",
					"syncthing",
					"--dir",
					d.RemoteDir,
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
						MountPath: d.RemoteDir,
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
			//v := unstructured.Unstructured{}
			//_, _, err = clientgoscheme.Codecs.UniversalDecoder(object.Mapping.GroupVersionKind.GroupVersion()).Decode(marshal, nil, &v)
			//_, _, err = unstructured.UnstructuredJSONScheme.Decode(marshal, &object.Mapping.GroupVersionKind, &v)
			if err = unstructured.SetNestedField(u.Object, m, path...); err != nil {
				return err
			}
			if err = d.replaceRegistry(u); err != nil {
				return err
			}

			_, createErr := client.Resource(object.Mapping.Resource).Namespace(d.TargetNamespace).Create(context.Background(), u, metav1.CreateOptions{})
			//_, createErr := runtimeresource.NewHelper(object.Client, object.Mapping).Create(d.TargetNamespace, true, u)
			return createErr
		})
		if retryErr != nil {
			return fmt.Errorf("create clone for resource %s failed: %v", workload, retryErr)
		}
		log.Infof("Create clone resource %s/%s in target cluster", u.GetObjectKind().GroupVersionKind().GroupKind().String(), u.GetName())
		log.Infof("Wait for clone resource %s/%s to be ready", u.GetObjectKind().GroupVersionKind().GroupKind().String(), u.GetName())
		log.Infoln()
		err = util.WaitPodToBeReady(ctx, d.targetClientset.CoreV1().Pods(d.TargetNamespace), metav1.LabelSelector{MatchLabels: labelsMap})
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

func (d *CloneOptions) SyncDir(ctx context.Context, labels string) error {
	list, err := util.GetRunningPodList(ctx, d.targetClientset, d.TargetNamespace, labels)
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
	log.Infof("Access the syncthing GUI via the following URL: %s", d.syncthingGUIAddr)
	go func() {
		client := syncthing.NewClient(localAddr)
		podName := list[0].Name
		for d.ctx.Err() == nil {
			func() {
				defer time.Sleep(time.Second * 2)

				sortBy := func(pods []*v1.Pod) sort.Interface { return sort.Reverse(podutils.ActivePods(pods)) }
				_, _, _ = polymorphichelpers.GetFirstPod(d.targetClientset.CoreV1(), d.TargetNamespace, labels, time.Second*30, sortBy)
				list, err := util.GetRunningPodList(d.ctx, d.targetClientset, d.TargetNamespace, labels)
				if err != nil {
					log.Error(err)
					return
				}
				if podName == list[0].Name {
					return
				}

				podName = list[0].Name
				log.Debugf("Detect newer pod %s", podName)
				var conf *libconfig.Configuration
				conf, err = client.GetConfig(d.ctx)
				if err != nil {
					log.Errorf("Failed to get config from syncthing: %v", err)
					return
				}
				for i := range conf.Devices {
					if config.RemoteDeviceID.Equals(conf.Devices[i].DeviceID) {
						addr := netutil.AddressURL("tcp", net.JoinHostPort(list[0].Status.PodIP, strconv.Itoa(libconfig.DefaultTCPPort)))
						conf.Devices[i].Addresses = []string{addr}
						log.Debugf("Use newer remote syncthing endpoint: %s", addr)
					}
				}
				err = client.PutConfig(d.ctx, conf)
				if err != nil {
					log.Errorf("Failed to set config to syncthing: %v", err)
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

// setVolume
/*
1) calculate volume content, and download it into emptyDir
*/
func (d *CloneOptions) setVolume(u *unstructured.Unstructured) error {

	const TokenVolumeMountPath = "/var/run/secrets/kubernetes.io/serviceaccount"

	type VolumeMountContainerPair struct {
		container   v1.Container
		volumeMount v1.VolumeMount
	}
	temp, path, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	lab := labels.SelectorFromSet(temp.Labels).String()
	var list []v1.Pod
	list, err = util.GetRunningPodList(context.Background(), d.clientset, d.Namespace, lab)
	if err != nil {
		return err
	}
	pod := list[0]

	// remove serviceAccount info
	temp.Spec.ServiceAccountName = ""
	temp.Spec.AutomountServiceAccountToken = pointer.Bool(false)

	var volumeMap = make(map[string]v1.Volume)
	var volumeList []v1.Volume
	// pod's volume maybe more than spec defined
	for _, volume := range pod.Spec.Volumes {
		volumeMap[volume.Name] = volume

		// keep volume emptyDir
		if volume.EmptyDir != nil {
			volumeList = append(volumeList, volume)
		} else {
			volumeList = append(volumeList, v1.Volume{
				Name: volume.Name,
				VolumeSource: v1.VolumeSource{
					EmptyDir: &v1.EmptyDirVolumeSource{},
				},
			})
		}
	}

	var tokenVolume string
	var volumeM = make(map[string][]VolumeMountContainerPair)
	for _, container := range pod.Spec.Containers {
		// group by volume name, what we want is figure out what's contains in every volume
		// we need to restore a total volume base on mountPath and subPath
		for _, volumeMount := range container.VolumeMounts {
			if volumeMap[volumeMount.Name].EmptyDir != nil {
				continue
			}
			if volumeMount.MountPath == TokenVolumeMountPath {
				tokenVolume = volumeMount.Name
			}
			mounts := volumeM[volumeMount.Name]
			if mounts == nil {
				volumeM[volumeMount.Name] = []VolumeMountContainerPair{}
			}
			volumeM[volumeMount.Name] = append(volumeM[volumeMount.Name], VolumeMountContainerPair{
				container:   container,
				volumeMount: volumeMount,
			})
		}
	}

	var initContainer []v1.Container
	for _, volume := range pod.Spec.Volumes {
		mountPoint := "/tmp/" + volume.Name
		var args []string
		for _, pair := range volumeM[volume.Name] {
			remote := filepath.Join(pair.volumeMount.MountPath, pair.volumeMount.SubPath)
			local := filepath.Join(mountPoint, pair.volumeMount.SubPath)
			// kubectl cp <some-namespace>/<some-pod>:/tmp/foo /tmp/bar
			args = append(args,
				fmt.Sprintf("kubevpn cp %s/%s:%s %s -c %s", pod.Namespace, pod.Name, remote, local, pair.container.Name),
			)
		}
		// means maybe volume only used in initContainers
		if len(args) == 0 {
			for i := 0; i < len(temp.Spec.InitContainers); i++ {
				for _, mount := range temp.Spec.InitContainers[i].VolumeMounts {
					if mount.Name == volume.Name {
						// remove useless initContainer
						temp.Spec.InitContainers = append(temp.Spec.InitContainers[:i], temp.Spec.InitContainers[i+1:]...)
						i--
						break
					}
				}
			}
			continue
		}
		newContainer := v1.Container{
			Name:       fmt.Sprintf("download-" + volume.Name),
			Image:      config.Image,
			Command:    []string{"sh", "-c"},
			Args:       []string{strings.Join(args, "&&")},
			WorkingDir: "/tmp",
			Env: []v1.EnvVar{
				{
					Name:  clientcmd.RecommendedConfigPathEnvVar,
					Value: "/tmp/.kube/kubeconfig",
				},
			},
			Resources: v1.ResourceRequirements{},
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      volume.Name,
					MountPath: mountPoint,
				},
				{
					Name:      config.KUBECONFIG,
					ReadOnly:  false,
					MountPath: "/tmp/.kube",
				},
			},
			ImagePullPolicy: v1.PullIfNotPresent,
		}
		initContainer = append(initContainer, newContainer)
	}
	// put download volume to front
	temp.Spec.InitContainers = append(initContainer, temp.Spec.InitContainers...)
	// replace old one
	temp.Spec.Volumes = volumeList
	// remove containers vpn and envoy-proxy
	inject.RemoveContainers(temp)
	// add each container service account token
	if tokenVolume != "" {
		for i := 0; i < len(temp.Spec.Containers); i++ {
			var found bool
			for _, mount := range temp.Spec.Containers[i].VolumeMounts {
				if mount.MountPath == TokenVolumeMountPath {
					found = true
					break
				}
			}
			if !found {
				temp.Spec.Containers[i].VolumeMounts = append(temp.Spec.Containers[i].VolumeMounts, v1.VolumeMount{
					Name:      tokenVolume,
					MountPath: TokenVolumeMountPath,
				})
			}
		}
	}
	var marshal []byte
	if marshal, err = json.Marshal(temp.Spec); err != nil {
		return err
	}
	var content map[string]interface{}
	if err = json.Unmarshal(marshal, &content); err != nil {
		return err
	}
	if err = unstructured.SetNestedField(u.Object, content, append(path, "spec")...); err != nil {
		return err
	}
	return nil
}

func (d *CloneOptions) setEnv(u *unstructured.Unstructured) error {
	temp, path, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	/*sortBy := func(pods []*v1.Pod) sort.Interface {
		for i := 0; i < len(pods); i++ {
			if pods[i].DeletionTimestamp != nil {
				pods = append(pods[:i], pods[i+1:]...)
				i--
			}
		}
		return sort.Reverse(podutils.ActivePods(pods))
	}
	lab := labels.SelectorFromSet(temp.Labels).String()
	pod, _, err := polymorphichelpers.GetFirstPod(d.clientset.CoreV1(), d.Namespace, lab, time.Second*60, sortBy)
	if err != nil {
		return err
	}

	var envMap map[string][]string
	envMap, err = util.GetEnv(context.Background(), d.factory, d.Namespace, pod.Name)
	if err != nil {
		return err
	}*/

	var secretMap = make(map[string]*v1.Secret)
	var configmapMap = make(map[string]*v1.ConfigMap)

	var howToGetCm = func(name string) {
		if configmapMap[name] == nil {
			cm, err := d.clientset.CoreV1().ConfigMaps(d.Namespace).Get(context.Background(), name, metav1.GetOptions{})
			if err == nil {
				configmapMap[name] = cm
			}
		}
	}
	var howToGetSecret = func(name string) {
		if configmapMap[name] == nil {
			secret, err := d.clientset.CoreV1().Secrets(d.Namespace).Get(context.Background(), name, metav1.GetOptions{})
			if err == nil {
				secretMap[name] = secret
			}
		}
	}

	// get all ref configmaps and secrets
	for _, container := range temp.Spec.Containers {
		for _, envVar := range container.Env {
			if envVar.ValueFrom != nil {
				if ref := envVar.ValueFrom.ConfigMapKeyRef; ref != nil {
					howToGetCm(ref.Name)
				}
				if ref := envVar.ValueFrom.SecretKeyRef; ref != nil {
					howToGetSecret(ref.Name)
				}
			}
		}
		for _, source := range container.EnvFrom {
			if ref := source.ConfigMapRef; ref != nil {
				if configmapMap[ref.Name] == nil {
					howToGetCm(ref.Name)
				}
			}
			if ref := source.SecretRef; ref != nil {
				howToGetSecret(ref.Name)
			}
		}
	}

	// parse real value from secrets and configmaps
	for i := 0; i < len(temp.Spec.Containers); i++ {
		container := temp.Spec.Containers[i]
		var envVars []v1.EnvVar
		for _, envFromSource := range container.EnvFrom {
			if ref := envFromSource.ConfigMapRef; ref != nil && configmapMap[ref.Name] != nil {
				cm := configmapMap[ref.Name]
				for k, v := range cm.Data {
					if strings.HasPrefix(k, envFromSource.Prefix) {
						envVars = append(envVars, v1.EnvVar{
							Name:  k,
							Value: v,
						})
					}
				}
			}
			if ref := envFromSource.SecretRef; ref != nil && secretMap[ref.Name] != nil {
				secret := secretMap[ref.Name]
				for k, v := range secret.Data {
					if strings.HasPrefix(k, envFromSource.Prefix) {
						envVars = append(envVars, v1.EnvVar{
							Name:  k,
							Value: string(v),
						})
					}
				}
			}
		}
		temp.Spec.Containers[i].EnvFrom = nil
		temp.Spec.Containers[i].Env = append(temp.Spec.Containers[i].Env, envVars...)

		for j, envVar := range container.Env {
			if envVar.ValueFrom != nil {
				if ref := envVar.ValueFrom.ConfigMapKeyRef; ref != nil {
					if configMap := configmapMap[ref.Name]; configMap != nil {
						temp.Spec.Containers[i].Env[j].Value = configMap.Data[ref.Key]
						temp.Spec.Containers[i].Env[j].ValueFrom = nil
					}
				}
				if ref := envVar.ValueFrom.SecretKeyRef; ref != nil {
					if secret := secretMap[ref.Name]; secret != nil {
						temp.Spec.Containers[i].Env[j].Value = string(secret.Data[ref.Key])
						temp.Spec.Containers[i].Env[j].ValueFrom = nil
					}
				}
			}
		}
	}
	var marshal []byte
	if marshal, err = json.Marshal(temp.Spec); err != nil {
		return err
	}
	var content map[string]interface{}
	if err = json.Unmarshal(marshal, &content); err != nil {
		return err
	}
	if err = unstructured.SetNestedField(u.Object, content, append(path, "spec")...); err != nil {
		return err
	}
	return nil
}

// replace origin registry with special registry for pulling image
func (d *CloneOptions) replaceRegistry(u *unstructured.Unstructured) error {
	// not pass this options, do nothing
	if !d.IsChangeTargetRegistry {
		return nil
	}

	temp, path, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	for i, container := range temp.Spec.InitContainers {
		oldImage := container.Image
		named, err := reference.ParseNormalizedNamed(oldImage)
		if err != nil {
			return err
		}
		domain := reference.Domain(named)
		newImage := strings.TrimPrefix(strings.ReplaceAll(oldImage, domain, d.TargetRegistry), "/")
		temp.Spec.InitContainers[i].Image = newImage
		log.Debugf("Update init container: %s image: %s --> %s", container.Name, oldImage, newImage)
	}

	for i, container := range temp.Spec.Containers {
		oldImage := container.Image
		named, err := reference.ParseNormalizedNamed(oldImage)
		if err != nil {
			return err
		}
		domain := reference.Domain(named)
		newImage := strings.TrimPrefix(strings.ReplaceAll(oldImage, domain, d.TargetRegistry), "/")
		temp.Spec.Containers[i].Image = newImage
		log.Debugf("Update container: %s image: %s --> %s", container.Name, oldImage, newImage)
	}

	var marshal []byte
	if marshal, err = json.Marshal(temp.Spec); err != nil {
		return err
	}
	var content map[string]interface{}
	if err = json.Unmarshal(marshal, &content); err != nil {
		return err
	}
	if err = unstructured.SetNestedField(u.Object, content, append(path, "spec")...); err != nil {
		return err
	}

	return nil
}

func (d *CloneOptions) Cleanup(workloads ...string) error {
	if len(workloads) == 0 {
		workloads = d.Workloads
	}
	for _, workload := range workloads {
		log.Infof("Cleaning up clone workload: %s", workload)
		object, err := util.GetUnstructuredObject(d.factory, d.Namespace, workload)
		if err != nil {
			log.Errorf("Failed to get unstructured object error: %s", err.Error())
			return err
		}
		labelsMap := map[string]string{
			config.ManageBy:   config.ConfigMapPodTrafficManager,
			"origin-workload": object.Name,
		}
		selector := labels.SelectorFromSet(labelsMap)
		controller, err := util.GetTopOwnerReferenceBySelector(d.targetFactory, d.TargetNamespace, selector.String())
		if err != nil {
			log.Errorf("Failed to get controller error: %s", err.Error())
			return err
		}
		var client dynamic.Interface
		client, err = d.targetFactory.DynamicClient()
		if err != nil {
			log.Errorf("Failed to get dynamic client error: %s", err.Error())
			return err
		}
		for _, cloneName := range controller.UnsortedList() {
			split := strings.Split(cloneName, "/")
			if len(split) == 2 {
				cloneName = split[1]
			}
			err = client.Resource(object.Mapping.Resource).Namespace(d.TargetNamespace).Delete(context.Background(), cloneName, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				log.Errorf("Failed to delete clone object: %v", err)
				return err
			}
			log.Infof("Deleted clone object: %s", cloneName)
		}
		log.Debugf("Cleanup clone workload: %s successfully", workload)
	}
	for _, f := range d.rollbackFuncList {
		if f != nil {
			if err := f(); err != nil {
				log.Warnf("Failed to exec rollback function: %s", err)
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
