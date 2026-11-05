package manager

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	pkgruntime "k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/cmd/set"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"

	"github.com/wencaiwulue/kubevpn/v2/pkg/cluster/workload"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// Images returns both desired and running images. During rollback these may
// differ, so the caller must check compatibility against both before upgrading.
func Images(ctx context.Context, clientset kubernetes.Interface, namespace string) (deploymentImage, runningPodImage string, err error) {
	deploy, err := clientset.AppsV1().Deployments(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		return "", "", fmt.Errorf("cannot find any container in deploy %s", deploy.Name)
	}
	pods, err := List(ctx, clientset, namespace)
	if err != nil {
		return "", "", err
	}
	if len(pods[0].Spec.Containers) == 0 {
		return "", "", fmt.Errorf("cannot find any container in pod %s", pods[0].Name)
	}
	return deploy.Spec.Template.Spec.Containers[0].Image, pods[0].Spec.Containers[0].Image, nil
}

// Upgrade applies an image selected and compatibility-checked by the caller.
// Deployment rollout failures are undone before the error is returned.
func Upgrade(ctx context.Context, f cmdutil.Factory, namespace, image string) error {
	if err := upgradeSecretSpec(ctx, f, namespace); err != nil {
		return err
	}
	if err := upgradeDeploySpec(ctx, f, namespace, config.ConfigMapPodTrafficManager, image); err != nil {
		return err
	}
	return upgradeServiceSpec(ctx, f, namespace)
}

func upgradeDeploySpec(ctx context.Context, f cmdutil.Factory, ns, name, image string) error {
	r := f.NewBuilder().
		WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
		NamespaceParam(ns).DefaultNamespace().
		ResourceNames("deployments", name).
		ContinueOnError().
		Latest().
		Flatten().
		Do()
	if err := r.Err(); err != nil {
		return err
	}
	infos, err := r.Infos()
	if err != nil {
		return err
	}
	// issue: https://github.com/kubernetes/kubernetes/issues/98963
	for _, info := range infos {
		_, _ = polymorphichelpers.UpdatePodSpecForObjectFn(info.Object, func(spec *v1.PodSpec) error {
			for i := 0; i < len(spec.ImagePullSecrets); i++ {
				if spec.ImagePullSecrets[i].Name == "" {
					spec.ImagePullSecrets = append(spec.ImagePullSecrets[:i], spec.ImagePullSecrets[i+1:]...)
					i--
					continue
				}
			}
			if len(spec.ImagePullSecrets) == 0 {
				spec.ImagePullSecrets = nil
			}
			return nil
		})
	}
	patches := set.CalculatePatches(infos, scheme.DefaultJSONEncoder(), func(obj pkgruntime.Object) ([]byte, error) {
		_, err = polymorphichelpers.UpdatePodSpecForObjectFn(obj, func(spec *v1.PodSpec) error {
			var imagePullSecret string
			for _, secret := range spec.ImagePullSecrets {
				if secret.Name != "" {
					imagePullSecret = secret.Name
					break
				}
			}
			deploySpec := genDeploySpec(ns, image, imagePullSecret)
			*spec = deploySpec.Spec.Template.Spec
			return nil
		})
		if err != nil {
			return nil, err
		}
		return pkgruntime.Encode(scheme.DefaultJSONEncoder(), obj)
	})
	for _, p := range patches {
		if p.Err != nil {
			return p.Err
		}
	}
	for _, p := range patches {
		_, err = resource.
			NewHelper(p.Info.Client, p.Info.Mapping).
			DryRun(false).
			Patch(p.Info.Namespace, p.Info.Name, k8stypes.StrategicMergePatchType, p.Patch, nil)
		if err != nil {
			plog.G(ctx).Errorf("Failed to patch image update to pod template: %v", err)
			return err
		}
		err = workload.RolloutStatus(ctx, f, ns, fmt.Sprintf("%s/%s", p.Info.Mapping.Resource.GroupResource().String(), p.Info.Name), true)
		if err != nil {
			return err
		}
	}
	return nil
}

func upgradeSecretSpec(ctx context.Context, f cmdutil.Factory, ns string) error {
	clientset, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}
	currentSecret, err := clientset.CoreV1().Secrets(ns).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}
	// Preserve existing complete certificates, including connections using them.
	if currentSecret.Data[config.TLSServerName] != nil &&
		currentSecret.Data[config.TLSPrivateKeyKey] != nil &&
		currentSecret.Data[config.TLSCertKey] != nil {
		return nil
	}
	return rotateTLSSecret(ctx, clientset, ns, currentSecret.ResourceVersion)
}

// RotateTLS replaces the traffic manager's TLS certificate, preserving the
// Secret's resource version for an optimistic-concurrency-safe update.
func RotateTLS(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	current, err := clientset.CoreV1().Secrets(namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}
	return rotateTLSSecret(ctx, clientset, namespace, current.ResourceVersion)
}

func rotateTLSSecret(ctx context.Context, clientset kubernetes.Interface, namespace, resourceVersion string) error {
	crt, key, host, err := netutil.GenTLSCert(ctx, namespace)
	if err != nil {
		return err
	}
	secret := genSecret(namespace, crt, key, host)
	secret.ResourceVersion = resourceVersion
	_, err = clientset.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
	return err
}

func upgradeServiceSpec(ctx context.Context, f cmdutil.Factory, ns string) error {
	svcSpec := genService(ns)
	clientset, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}
	currentSvc, err := clientset.CoreV1().Services(ns).Get(ctx, svcSpec.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	svcSpec.ResourceVersion = currentSvc.ResourceVersion
	_, err = clientset.CoreV1().Services(ns).Update(ctx, svcSpec, metav1.UpdateOptions{})
	return err
}
