package handler

import (
	"context"
	"fmt"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	pkgruntime "k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/kubectl/pkg/cmd/set"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (c *ConnectOptions) upgradeDeploy(ctx context.Context) error {
	deploy, err := c.clientset.AppsV1().Deployments(c.ManagerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		return fmt.Errorf("cannot find any container in deploy %s", deploy.Name)
	}
	// check running pod, sometime deployment is rolling back, so need to check running pod
	podList, err := c.GetRunningPodList(ctx)
	if err != nil {
		return err
	}

	clientVer := config.Version
	clientImg := c.Image
	serverImg := deploy.Spec.Template.Spec.Containers[0].Image
	runningPodImg := podList[0].Spec.Containers[0].Image

	isNeedUpgrade, err := util.IsNewer(clientVer, clientImg, serverImg)
	if err != nil {
		return err
	}
	isPodNeedUpgrade, err := util.IsNewer(clientVer, clientImg, runningPodImg)
	if err != nil {
		return err
	}
	if !isNeedUpgrade && !isPodNeedUpgrade {
		return nil
	}

	// 1) update secret
	err = upgradeSecretSpec(ctx, c.factory, c.ManagerNamespace)
	if err != nil {
		return err
	}

	// 2) update deploy
	plog.G(ctx).Infof("Set image %s --> %s...", serverImg, clientImg)
	err = upgradeDeploySpec(ctx, c.factory, c.ManagerNamespace, deploy.Name, clientImg)
	if err != nil {
		return err
	}
	// 3) update service
	err = upgradeServiceSpec(ctx, c.factory, c.ManagerNamespace)
	if err != nil {
		return err
	}
	return nil
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
		err = util.RolloutStatus(ctx, f, ns, fmt.Sprintf("%s/%s", p.Info.Mapping.Resource.GroupResource().String(), p.Info.Name))
		if err != nil {
			return err
		}
	}
	return nil
}

func upgradeSecretSpec(ctx context.Context, f cmdutil.Factory, ns string) error {
	crt, key, host, err := util.GenTLSCert(ctx, ns)
	if err != nil {
		return err
	}
	secret := genSecret(ns, crt, key, host)

	clientset, err := f.KubernetesClientSet()
	if err != nil {
		return err
	}
	currentSecret, err := clientset.CoreV1().Secrets(ns).Get(ctx, secret.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// already have three keys
	if currentSecret.Data[config.TLSServerName] != nil &&
		currentSecret.Data[config.TLSPrivateKeyKey] != nil &&
		currentSecret.Data[config.TLSCertKey] != nil {
		return nil
	}

	_, err = clientset.CoreV1().Secrets(ns).Update(ctx, secret, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	mutatingWebhookConfig := genMutatingWebhookConfiguration(ns, crt)
	var current *admissionv1.MutatingWebhookConfiguration
	current, err = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, mutatingWebhookConfig.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	mutatingWebhookConfig.ResourceVersion = current.ResourceVersion
	_, err = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Update(ctx, mutatingWebhookConfig, metav1.UpdateOptions{})
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
