package handler

import (
	"bytes"
	"context"
	"errors"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func createOutboundPod(ctx context.Context, clientset kubernetes.Interface, namespace, image, imagePullSecretName string) (err error) {
	var exists bool
	exists, err = util.DetectPodExists(ctx, clientset, namespace)
	if err != nil {
		return err
	}
	if exists {
		plog.StepDone(ctx, "Using traffic manager in namespace %q", namespace)
		return nil
	}

	plog.StepStart(ctx, "Creating traffic manager")
	defer func() {
		if err != nil {
			cleanupTrafficManagerResources(context.Background(), clientset, namespace)
		}
	}()
	cleanupTrafficManagerResources(ctx, clientset, namespace)

	// 1) label namespace
	plog.G(ctx).Infof("Labeling Namespace %s", namespace)
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Get Namespace error: %v", err)
		return err
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels["ns"] = namespace
	_, err = clientset.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
	if err != nil {
		plog.G(ctx).Infof("Labeling Namespace error: %v", err)
		return err
	}

	// 2) create serviceAccount
	plog.G(ctx).Infof("Creating ServiceAccount %s", config.ConfigMapPodTrafficManager)
	_, err = clientset.CoreV1().ServiceAccounts(namespace).Create(ctx, genServiceAccount(namespace), metav1.CreateOptions{})
	if err != nil {
		plog.G(ctx).Infof("Creating ServiceAccount error: %v", err)
		return err
	}

	// 3) create roles
	plog.G(ctx).Infof("Creating Roles %s", config.ConfigMapPodTrafficManager)
	_, err = clientset.RbacV1().Roles(namespace).Create(ctx, genRole(namespace), metav1.CreateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Creating Roles error: %v", err)
		return err
	}

	// 4) create roleBinding
	plog.G(ctx).Infof("Creating RoleBinding %s", config.ConfigMapPodTrafficManager)
	_, err = clientset.RbacV1().RoleBindings(namespace).Create(ctx, genRoleBinding(namespace), metav1.CreateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Creating RoleBinding error: %v", err)
		return err
	}

	// 5) create service
	plog.G(ctx).Infof("Creating Service %s", config.ConfigMapPodTrafficManager)
	svcSpec := genService(namespace)
	_, err = clientset.CoreV1().Services(namespace).Create(ctx, svcSpec, metav1.CreateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Creating Service error: %v", err)
		return err
	}

	plog.G(ctx).Infof("Creating ConfigMap %s", config.ConfigMapPodTrafficManager)
	_, err = clientset.CoreV1().ConfigMaps(namespace).Create(ctx, &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: namespace,
		},
		Data: map[string]string{
			config.KeyEnvoy:       "",
			config.KeyTunIPPool:   "",
			config.KeyClusterCIDRs: "",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Creating ConfigMap error: %v", err)
		return err
	}

	_, err = createTLSSecret(ctx, clientset, namespace)
	if err != nil {
		return err
	}

	// 6) create deployment
	plog.G(ctx).Infof("Creating Deployment %s", config.ConfigMapPodTrafficManager)
	deploy := genDeploySpec(namespace, image, imagePullSecretName)
	deploy, err = clientset.AppsV1().Deployments(namespace).Create(ctx, deploy, metav1.CreateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Failed to create deployment for %s: %v", config.ConfigMapPodTrafficManager, err)
		return err
	}

	_, selector, err := polymorphichelpers.SelectorsForObject(deploy)
	if err != nil {
		return err
	}
	if err = WaitPodReady(ctx, clientset.CoreV1().Pods(namespace), selector.String()); err != nil {
		return err
	}
	plog.StepDone(ctx, "Created traffic manager in namespace %q", namespace)
	return nil
}

func WaitPodReady(ctx context.Context, clientset corev1.PodInterface, labelSelector string) error {
	const (
		podReadyTimeout      = 60 * time.Minute
		podReadyPollInterval = 3 * time.Second
	)
	var isPodReady bool
	var lastMessage string
	ctx2, cancelFunc := context.WithTimeout(ctx, podReadyTimeout)
	defer cancelFunc()
	plog.G(ctx).Infoln()
	wait.UntilWithContext(ctx2, func(ctx context.Context) {
		podList, err := clientset.List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return
		}
		if len(podList.Items) == 0 {
			return
		}
		for _, pod := range podList.Items {
			if podutils.IsPodReady(&pod) && pod.DeletionTimestamp == nil {
				isPodReady = true
				cancelFunc()
				return
			}
			var sb bytes.Buffer
			util.PrintStatus(&pod, &sb)
			newMsg := sb.String()
			if newMsg != lastMessage {
				lastMessage = newMsg
				plog.G(ctx).Infof(newMsg)
			}
		}
	}, podReadyPollInterval)
	if !isPodReady {
		return errors.New("wait for pod ready timeout")
	}
	return nil
}

// createTLSSecret generates a TLS certificate and creates the corresponding
// Secret in the cluster.
// Note: v1.SecretTypeOpaque is used instead of v1.SecretTypeTls because TLS
// secret keys (tls.crt, tls.key) contain dots which are invalid as shell
// environment variable names, and the secret is mounted as env vars.
func createTLSSecret(ctx context.Context, clientset kubernetes.Interface, namespace string) (crt []byte, err error) {
	var key, host []byte
	crt, key, host, err = util.GenTLSCert(ctx, namespace)
	if err != nil {
		return nil, err
	}
	_, err = clientset.CoreV1().Secrets(namespace).Create(ctx, genSecret(namespace, crt, key, host), metav1.CreateOptions{})
	if err != nil {
		plog.G(ctx).Errorf("Creating secret error: %v", err)
		return nil, err
	}
	return crt, nil
}

func cleanupTrafficManagerResources(ctx context.Context, clientset kubernetes.Interface, namespace string) {
	options := metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}
	name := config.ConfigMapPodTrafficManager
	_ = clientset.RbacV1().RoleBindings(namespace).Delete(ctx, name, options)
	_ = clientset.RbacV1().Roles(namespace).Delete(ctx, name, options)
	_ = clientset.CoreV1().ServiceAccounts(namespace).Delete(ctx, name, options)
	_ = clientset.CoreV1().Services(namespace).Delete(ctx, name, options)
	_ = clientset.CoreV1().ConfigMaps(namespace).Delete(ctx, name, options)
	_ = clientset.CoreV1().Secrets(namespace).Delete(ctx, name, options)
	_ = clientset.CoreV1().Pods(namespace).Delete(ctx, config.CniNetName, options)
	_ = clientset.BatchV1().Jobs(namespace).Delete(ctx, name, options)
	_ = clientset.AppsV1().Deployments(namespace).Delete(ctx, name, options)
}
