package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	runtimeresource "k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/clientcmd/api/latest"
	clientcmdlatest "k8s.io/client-go/tools/clientcmd/api/latest"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/mesh"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

type DuplicateOptions struct {
	Namespace string
	Headers   map[string]string
	Workloads []string

	TargetKubeconfig string
	TargetNamespace  string
	TargetContainer  string
	TargetImage      string
	TargetRegistry   string

	isSame bool

	targetClientset  *kubernetes.Clientset
	targetRestclient *rest.RESTClient
	targetConfig     *rest.Config
	targetFactory    cmdutil.Factory

	clientset  *kubernetes.Clientset
	restclient *rest.RESTClient
	config     *rest.Config
	factory    cmdutil.Factory
}

func (d *DuplicateOptions) InitClient(f cmdutil.Factory) (err error) {
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
	loader := d.targetFactory.ToRawKubeConfigLoader()
	var found bool
	d.TargetNamespace, found, err = loader.Namespace()
	if err != nil || !found {
		d.TargetNamespace = d.Namespace
	}
	d.targetClientset, err = d.targetFactory.KubernetesClientSet()
	return
}

// DoDuplicate
/*
* 1) download mount path use empty-dir but skip empty-dir in init-containers
* 2) get env from containers
* 3) create serviceAccount as needed
* 4) modify podTempSpec inject kubevpn container
 */
func (d *DuplicateOptions) DoDuplicate(ctx context.Context) error {
	rawConfig, err := d.targetFactory.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return err
	}
	err = api.FlattenConfig(&rawConfig)
	if err != nil {
		return err
	}
	rawConfig.SetGroupVersionKind(schema.GroupVersionKind{Version: clientcmdlatest.Version, Kind: "Config"})
	var convertedObj runtime.Object
	convertedObj, err = latest.Scheme.ConvertToVersion(&rawConfig, latest.ExternalVersion)
	if err != nil {
		return err
	}
	var kubeconfigJsonBytes []byte
	kubeconfigJsonBytes, err = json.Marshal(convertedObj)
	if err != nil {
		return err
	}

	for _, workload := range d.Workloads {
		var object *runtimeresource.Info
		object, err = util.GetUnstructuredObject(d.factory, d.Namespace, workload)
		if err != nil {
			return err
		}
		u := object.Object.(*unstructured.Unstructured)
		u.SetNamespace(d.TargetNamespace)
		RemoveUselessInfo(u)
		var newUUID uuid.UUID
		newUUID, err = uuid.NewUUID()
		if err != nil {
			return err
		}
		u.SetName(fmt.Sprintf("%s-dup-%s", u.GetName(), newUUID.String()[:5]))

		// if is another cluster, needs to set volume and set env
		if !d.isSame {
			if err = d.setVolume(u); err != nil {
				return err
			}
			if err = d.setEnv(u); err != nil {
				return err
			}
		}
		d.replaceRegistry(u)

		labelsMap := map[string]string{
			config.ManageBy: config.ConfigMapPodTrafficManager,
			"owner-ref":     u.GetName(),
		}
		var path []string
		_, path, err = util.GetPodTemplateSpecPath(u)
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
		RollbackFuncList = append(RollbackFuncList, func() {
			_ = client.Resource(object.Mapping.Resource).Namespace(d.TargetNamespace).Delete(context.Background(), u.GetName(), metav1.DeleteOptions{})
		})
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var volumesPath = append(path, "spec", "volumes")
			var containersPath = append(path, "spec", "containers")
			var annotationPath = append(path, "metadata", "annotations")
			var labelsPath = append(path, "metadata", "labels")

			// (1) add annotation KUBECONFIG
			stringMap, found, err := unstructured.NestedStringMap(u.Object, annotationPath...)
			if err != nil {
				return err
			}
			if !found {
				stringMap = map[string]string{}
			}
			stringMap[config.KUBECONFIG] = string(kubeconfigJsonBytes)
			if err = unstructured.SetNestedStringMap(u.Object, stringMap, annotationPath...); err != nil {
				return err
			}

			// (2) modify labels
			if err = unstructured.SetNestedStringMap(u.Object, labelsMap, labelsPath...); err != nil {
				return err
			}

			// (3) add volumes KUBECONFIG
			volumes, found, err := unstructured.NestedSlice(u.Object, volumesPath...)
			if err != nil {
				return err
			}
			if !found {
				volumes = []interface{}{}
			}
			volume := &v1.Volume{
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
			}
			marshal, err := json.Marshal(volume)
			v := unstructured.Unstructured{}
			err = v.UnmarshalJSON(marshal)
			if err = unstructured.SetNestedSlice(u.Object, append(volumes, v.Object), volumesPath...); err != nil {
				return err
			}

			// (4) add kubevpn containers
			containers, found, err := unstructured.NestedSlice(u.Object, containersPath...)
			if err != nil || !found || containers == nil {
				return fmt.Errorf("deployment containers not found or error in spec: %v", err)
			}
			if d.TargetImage != "" {
				var index = -1
				if d.TargetContainer != "" {
					for i, container := range containers {
						nestedString, _, err := unstructured.NestedString(container.(map[string]interface{}), "name")
						if err == nil && nestedString == d.TargetContainer {
							index = i
							break
						}
					}
				} else {
					index = 0
				}
				if index < 0 {
					return fmt.Errorf("can not found container %s in pod template", d.TargetContainer)
				}
				// update container[index] image
				if err = unstructured.SetNestedField(containers[index].(map[string]interface{}), d.TargetImage, "image"); err != nil {
					return err
				}
			}
			container := &v1.Container{
				Name:  config.ContainerSidecarVPN,
				Image: config.Image,
				Command: []string{
					"kubevpn",
					"proxy",
					workload,
					"--kubeconfig", "/tmp/.kube/" + config.KUBECONFIG,
					"--namespace", d.Namespace,
					"--headers", labels.Set(d.Headers).String(),
					"--image", config.Image,
				},
				Args: nil,
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
				VolumeMounts: []v1.VolumeMount{{
					Name:      config.KUBECONFIG,
					ReadOnly:  false,
					MountPath: "/tmp/.kube",
				}},
				ImagePullPolicy: v1.PullIfNotPresent,
				SecurityContext: &v1.SecurityContext{
					Capabilities: &v1.Capabilities{
						Add: []v1.Capability{
							"NET_ADMIN",
						},
					},
					RunAsUser:  pointer.Int64(0),
					Privileged: pointer.Bool(true),
				},
			}
			marshal, err = json.Marshal(container)
			v = unstructured.Unstructured{}
			err = v.UnmarshalJSON(marshal)
			if err = unstructured.SetNestedField(u.Object, append(containers, v.Object), containersPath...); err != nil {
				return err
			}

			_, createErr := client.Resource(object.Mapping.Resource).Namespace(d.TargetNamespace).Create(context.Background(), u, metav1.CreateOptions{})
			//_, createErr := runtimeresource.NewHelper(object.Client, object.Mapping).Create(d.TargetNamespace, true, u)
			return createErr
		})
		if retryErr != nil {
			return fmt.Errorf("create duplidate for resource %s failed: %v", workload, retryErr)
		}
		err = util.WaitPodToBeReady(ctx, d.targetClientset.CoreV1().Pods(d.TargetNamespace), metav1.LabelSelector{MatchLabels: labelsMap})
		if err != nil {
			return err
		}
		_ = util.RolloutStatus(ctx, d.factory, d.Namespace, workload, time.Minute*60)
	}
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
func (d *DuplicateOptions) setVolume(u *unstructured.Unstructured) error {

	const TokenVolumeMountPath = "/var/run/secrets/kubernetes.io/serviceaccount"

	type VolumeMountContainerPair struct {
		container   v1.Container
		volumeMount v1.VolumeMount
	}
	temp, path, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	sortBy := func(pods []*v1.Pod) sort.Interface {
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

	// remove serviceAccount info
	temp.Spec.ServiceAccountName = ""
	temp.Spec.DeprecatedServiceAccount = ""
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
	mesh.RemoveContainers(temp)
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

func (d *DuplicateOptions) setEnv(u *unstructured.Unstructured) error {
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

// todo replace origin registry with special registry for pulling image
func (d *DuplicateOptions) replaceRegistry(u *unstructured.Unstructured) {
	if d.TargetRegistry != "" {

	}
}
