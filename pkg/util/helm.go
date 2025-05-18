package util

import (
	"context"
	"os"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/release/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// DetectManagerNamespace
//  1. use helm to install kubevpn server, means cluster mode,
//     all kubevpn client should connect to this namespace.
//  2. if any error occurs, just ignore and will use options `-n` or `--namespace`
func DetectManagerNamespace(ctx context.Context, f cmdutil.Factory, namespace string) (string, error) {
	clientSet, err := f.KubernetesClientSet()
	if err != nil {
		return "", err
	}

	var exists bool
	exists, err = DetectPodExists(ctx, clientSet, namespace)
	if err != nil && !k8serrors.IsNotFound(err) && !k8serrors.IsForbidden(err) {
		return "", err
	} else if err != nil {
		plog.G(ctx).Debugf("Failed to detect if kubevpn exists in namespace %s: %v", namespace, err)
	}
	if exists {
		plog.G(ctx).Debugf("Find exists kubevpn in namespace %s", namespace)
		return namespace, nil
	}

	exists, err = DetectPodExists(ctx, clientSet, config.DefaultNamespaceKubevpn)
	if err != nil && !k8serrors.IsNotFound(err) && !k8serrors.IsForbidden(err) {
		return "", err
	} else if err != nil {
		plog.G(ctx).Debugf("Failed to detect if kubevpn exists in namespace %s: %v", config.DefaultNamespaceKubevpn, err)
	}
	if exists {
		plog.G(ctx).Debugf("Find exists kubevpn in namespace %s", config.DefaultNamespaceKubevpn)
		return config.DefaultNamespaceKubevpn, nil
	}

	return GetHelmInstalledNamespace(ctx, f)
}

func GetHelmInstalledNamespace(ctx context.Context, f cmdutil.Factory) (string, error) {
	cfg := new(action.Configuration)
	client := action.NewList(cfg)
	err := cfg.Init(f, "", os.Getenv("HELM_DRIVER"), plog.G(ctx).Debugf)
	if err != nil {
		return "", err
	}
	client.SetStateMask()
	releases, err := client.Run()
	if err != nil {
		if k8serrors.IsForbidden(err) {
			plog.G(ctx).Debugf("Failed to list helm apps in all namespace: %v", err)
			return "", nil
		}
		return "", err
	}
	for _, app := range releases {
		if app.Name == config.HelmAppNameKubevpn &&
			app.Info != nil && app.Info.Status == v1.StatusDeployed {
			plog.G(ctx).Debugf("Find exists helm app kubevpn in namespace %s", app.Namespace)
			return app.Namespace, nil
		}
	}
	plog.G(ctx).Debugf("Not found helm apps kubevpn in all namespace")
	return "", nil
}

func DetectPodExists(ctx context.Context, clientset *kubernetes.Clientset, namespace string) (bool, error) {
	label := fields.OneTermEqualSelector("app", config.ConfigMapPodTrafficManager).String()
	list, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: label,
	})
	if err != nil {
		return false, err
	}
	for i := 0; i < len(list.Items); i++ {
		if list.Items[i].GetDeletionTimestamp() != nil || !AllContainerIsRunning(&list.Items[i]) {
			list.Items = append(list.Items[:i], list.Items[i+1:]...)
			i--
		}
	}
	if len(list.Items) == 0 {
		return false, nil
	}
	return true, nil
}
