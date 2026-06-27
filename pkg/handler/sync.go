package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/cmd/util/podcmd"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// SyncOptions holds configuration for syncthing-based file synchronization.
type SyncOptions struct {
	K8sClient

	Namespace      string
	Headers        map[string]string
	Workloads      []string
	ExtraRouteInfo ExtraRouteInfo

	TargetContainer     string
	TargetImage         string
	TargetWorkloadNames map[string]string

	OriginKubeconfigPath string
	LocalDir             string
	RemoteDir            string

	rollbackFuncList []func() error
	ctx              context.Context
	syncthingGUIAddr string
}

// InitClient initializes the Kubernetes client, REST client, and namespace from the given factory.
func (d *SyncOptions) InitClient(f cmdutil.Factory) error {
	var err error
	d.Namespace, err = d.K8sClient.InitClient(f)
	return err
}

// SetContext sets the background context used by long-running sync operations.
func (d *SyncOptions) SetContext(ctx context.Context) {
	d.ctx = ctx
}

// DoSync orchestrates syncthing-based file synchronization:
// 1) creates a sync workload (clone of the target with syncthing sidecar)
// 2) waits for it to become ready
// 3) optionally starts local syncthing client for directory sync
func (d *SyncOptions) DoSync(ctx context.Context, kubeconfigJsonBytes []byte, image string) error {
	var args []string
	if len(d.Headers) != 0 {
		args = append(args, "--headers", labels.Set(d.Headers).String())
	}
	for _, workload := range d.Workloads {
		plog.G(ctx).Infof("Sync workload %s", workload)
		_, controller, err := util.GetTopOwnerObject(ctx, d.factory, d.Namespace, workload)
		if err != nil {
			return err
		}
		u := controller.Object.(*unstructured.Unstructured)
		if err = unstructured.SetNestedField(u.UnstructuredContent(), int64(1), "spec", "replicas"); err != nil {
			plog.G(ctx).Warnf("Failed to set replicaset to 1: %v", err)
		}
		clearObjectMetadata(u)
		var newUUID uuid.UUID
		newUUID, err = uuid.NewUUID()
		if err != nil {
			return err
		}
		originName := u.GetName()
		u.SetName(fmt.Sprintf("%s-sync-%s", u.GetName(), newUUID.String()[:5]))
		d.TargetWorkloadNames[workload] = fmt.Sprintf("%s/%s", controller.Mapping.Resource.GroupResource().Resource, u.GetName())
		labelsMap := map[string]string{
			config.ManageBy:   config.ConfigMapPodTrafficManager,
			"owner-ref":       u.GetName(),
			"origin-workload": originName,
		}
		u.SetLabels(labels.Merge(u.GetLabels(), labelsMap))
		u.SetDeletionGracePeriodSeconds(ptr.To[int64](60))
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
			if err := d.prepareSyncPodSpec(ctx, syncPodSpec{
				spec:       spec,
				u:          u,
				workload:   workload,
				kubeconfig: kubeconfigJsonBytes,
				image:      image,
				args:       args,
				path:       path,
				labels:     labelsMap,
			}); err != nil {
				return err
			}
			_, createErr := client.Resource(controller.Mapping.Resource).Namespace(d.Namespace).Create(ctx, u, metav1.CreateOptions{})
			return createErr
		})
		if retryErr != nil {
			return fmt.Errorf("create sync for resource %s failed: %w", workload, retryErr)
		}
		plog.G(ctx).Infof("Create sync resource %s/%s in target cluster", u.GetObjectKind().GroupVersionKind().GroupKind().String(), u.GetName())
		plog.G(ctx).Infof("Wait for sync resource %s/%s to be ready", u.GetObjectKind().GroupVersionKind().GroupKind().String(), u.GetName())
		plog.G(ctx).Infoln()
		err = WaitPodReady(ctx, d.clientset.CoreV1().Pods(d.Namespace), fields.SelectorFromSet(labelsMap).String())
		if err != nil {
			return err
		}
		_ = util.RolloutStatus(ctx, d.factory, d.Namespace, workload)

		if d.LocalDir != "" {
			err = d.SyncDir(ctx, fields.SelectorFromSet(labelsMap).String())
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// prepareSyncPodSpec modifies the pod template spec for the sync resource by adding
// annotations, labels, volumes, and containers needed for syncthing-based file sync.
func (d *SyncOptions) prepareSyncPodSpec(ctx context.Context, s syncPodSpec) error {
	spec := s.spec
	anno := spec.GetAnnotations()
	if anno == nil {
		anno = map[string]string{}
	}
	anno[config.KUBECONFIG] = string(s.kubeconfig)
	spec.SetAnnotations(anno)

	spec.SetLabels(s.labels)

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

	containers := spec.Spec.Containers
	for i := 0; i < len(containers); i++ {
		containerName := containers[i].Name
		if containerName == config.ContainerSidecarVPN || containerName == config.ContainerSidecarEnvoyProxy {
			containers = append(containers[:i], containers[i+1:]...)
			i--
		}
	}
	{
		container, err := podcmd.FindOrDefaultContainerByName(&v1.Pod{Spec: v1.PodSpec{Containers: containers}}, d.TargetContainer, false, plog.G(ctx).Logger.Out)
		if err != nil {
			return err
		}
		if d.TargetImage != "" {
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
	if spec.Spec.SecurityContext == nil {
		spec.Spec.SecurityContext = &v1.PodSecurityContext{}
	}
	container := genVPNContainer(s.workload, d.Namespace, s.image, s.args)
	containerSync := genSyncthingContainer(d.RemoteDir, syncDataDirName, s.image)
	spec.Spec.Containers = append(containers, *container, *containerSync)

	marshal, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	m := make(map[string]any)
	err = json.Unmarshal(marshal, &m)
	if err != nil {
		return err
	}
	if err = unstructured.SetNestedField(s.u.Object, m, s.path...); err != nil {
		return err
	}
	return nil
}

// Cleanup deletes sync workloads created by DoSync and executes rollback functions.
// Deletion errors are logged but do not prevent rollback functions from running,
// ensuring resources are cleaned up even when the API server is partially unreachable.
func (d *SyncOptions) Cleanup(ctx context.Context, workloads ...string) error {
	if len(workloads) == 0 {
		for _, v := range d.TargetWorkloadNames {
			workloads = append(workloads, v)
		}
	}
	var firstErr error
	for _, workload := range workloads {
		plog.G(ctx).Infof("Cleaning up sync workload: %s", workload)
		object, err := util.GetUnstructuredObject(d.factory, d.Namespace, workload)
		if err != nil {
			plog.G(ctx).Errorf("Failed to get unstructured object: %v", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		var client dynamic.Interface
		client, err = d.factory.DynamicClient()
		if err != nil {
			plog.G(ctx).Errorf("Failed to get dynamic client: %v", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		err = client.Resource(object.Mapping.Resource).Namespace(d.Namespace).Delete(ctx, object.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			plog.G(ctx).Errorf("Failed to delete sync object: %v", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		plog.G(ctx).Infof("Deleted sync object: %s/%s", object.Mapping.Resource.GroupResource().Resource, object.Name)
	}
	for _, f := range d.rollbackFuncList {
		if f != nil {
			if err := f(); err != nil {
				plog.G(ctx).Warnf("Failed to exec rollback function: %s", err)
			}
		}
	}
	for _, workload := range d.Workloads {
		plog.G(ctx).Infof("Wait workload %s", workload)
		err := util.RolloutStatus(ctx, d.factory, d.Namespace, workload)
		if err != nil {
			plog.G(ctx).Warnf("Failed to rollback workload %s: %v", workload, err)
		}
	}
	return firstErr
}

// AddRollbackFunc registers a function to be called during Cleanup for teardown.
func (d *SyncOptions) AddRollbackFunc(f func() error) {
	d.rollbackFuncList = append(d.rollbackFuncList, f)
}

// GetSyncthingGUIAddr returns the local address of the syncthing GUI for status monitoring.
func (d *SyncOptions) GetSyncthingGUIAddr() string {
	return d.syncthingGUIAddr
}

// syncPodSpec bundles the parameters needed by prepareSyncPodSpec to modify
// a pod template for syncthing-based file sync.
type syncPodSpec struct {
	spec       *v1.PodTemplateSpec
	u          *unstructured.Unstructured
	workload   string
	kubeconfig []byte
	image      string
	args       []string
	path       []string
	labels     map[string]string
}
