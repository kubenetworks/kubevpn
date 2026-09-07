package handler

import (
	"context"
	"encoding/json"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/kubectl/pkg/cmd/util/podcmd"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

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
		if containerName == config.ContainerSidecarVPN || containerName == config.ContainerSidecarEnvoy {
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
	container := genVPNContainer(s.workload, d.WorkloadNamespace, d.ManagerNamespace, s.image, s.args)
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
